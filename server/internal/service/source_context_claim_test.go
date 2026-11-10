package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestSourceContextCleanupClaimUpdatesExactlyOneIntent(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	lockSourceContextCleanupTests(t, pool)
	workspace, _, _, _ := seedAttributionFixture(t, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE issue_source_context_object_intent IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_source_context_object_intent SET next_attempt_at=now()+interval '1 hour' WHERE next_attempt_at<=now()`); err != nil {
		t.Fatal(err)
	}
	// A nested loop without a materialize node can re-evaluate a LIMIT
	// subquery for each target row. The claim must stay single-row under
	// this valid plan as well as the planner's usual hash-join choice.
	for _, setting := range []string{"SET LOCAL enable_hashjoin=off", "SET LOCAL enable_mergejoin=off", "SET LOCAL enable_material=off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	q := db.New(tx)
	contextID := dbid.NewV7()
	for range 3 {
		attachment := dbid.NewV7()
		key := "source-context-claim/" + util.UUIDToString(attachment)
		if _, err := q.RecordSourceContextObjectIntent(ctx, db.RecordSourceContextObjectIntentParams{
			StorageKey: key, WorkspaceID: util.MustParseUUID(workspace), SourceContextID: contextID,
			AttachmentID: attachment, ObjectUrl: "https://objects.example/" + key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_source_context_object_intent SET created_at=now()-interval '2 hours',next_attempt_at='-infinity'::timestamptz WHERE source_context_id=$1`, contextID); err != nil {
		t.Fatal(err)
	}
	lease := dbid.NewV7()
	claimed, err := q.ClaimSourceContextObjectIntentForCleanup(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	var deleting, pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='deleting'), count(*) FILTER (WHERE state='pending') FROM issue_source_context_object_intent WHERE source_context_id=$1`, contextID).Scan(&deleting, &pending); err != nil {
		t.Fatal(err)
	}
	if deleting != 1 || pending != 2 || claimed.LeaseToken != lease {
		t.Fatalf("single claim changed deleting=%d pending=%d, want 1/2", deleting, pending)
	}
}
