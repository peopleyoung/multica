package daemon

import (
	"os"
	"path/filepath"
	"reflect"

	"testing"
)

func writeTestLocalSkill(t *testing.T, root, rel string, files map[string]string) string {
	t.Helper()

	skillDir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	for path, content := range files {
		fullPath := filepath.Join(skillDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir parents for %s: %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return skillDir
}

// ---------------------------------------------------------------------------
// Multi-root discovery: provider root + universal ~/.agents/skills (MUL-3333)
// ---------------------------------------------------------------------------

// A skill that lives only in the universal ~/.agents/skills root (no provider
// directory at all) must be discovered and tagged Root="universal".
func TestListRuntimeLocalSkills_DiscoversUniversalAgentsRoot(t *testing.T) {
	home := t.TempDir()
	redirectTestHome(t, home)

	writeTestLocalSkill(t, filepath.Join(home, ".agents", "skills"), "universal-helper", map[string]string{
		"SKILL.md":     "---\nname: Universal Helper\ndescription: Cross-tool skill\n---\n# Universal Helper\n",
		"docs/info.md": "info",
	})

	skills, supported, err := listRuntimeLocalSkills("native")
	if err != nil {
		t.Fatalf("listRuntimeLocalSkills: %v", err)
	}
	if !supported {
		t.Fatal("native should be supported")
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d (%v)", len(skills), skills)
	}
	if skills[0].Key != "universal-helper" {
		t.Fatalf("key = %q, want universal-helper", skills[0].Key)
	}
	if skills[0].Name != "Universal Helper" {
		t.Fatalf("name = %q, want Universal Helper", skills[0].Name)
	}
	if skills[0].Root != localSkillRootUniversal {
		t.Fatalf("root = %q, want %q", skills[0].Root, localSkillRootUniversal)
	}
	if skills[0].SourcePath != "~/.agents/skills/universal-helper" {
		t.Fatalf("source_path = %q", skills[0].SourcePath)
	}
	// 2 = supporting file (docs/info.md) + SKILL.md.
	if skills[0].FileCount != 2 {
		t.Fatalf("file_count = %d, want 2", skills[0].FileCount)
	}
}

// A skill discovered under ~/.agents/skills must be importable, not just
// listable — otherwise the import dialog shows a skill the load endpoint
// can't fetch.
func TestLoadRuntimeLocalSkillBundle_ImportsFromUniversalRoot(t *testing.T) {
	home := t.TempDir()
	redirectTestHome(t, home)

	writeTestLocalSkill(t, filepath.Join(home, ".agents", "skills"), "shared-skill", map[string]string{
		"SKILL.md":        "---\nname: Shared Skill\ndescription: Imported from agents root\n---\n# Shared Skill\n",
		"examples/use.md": "usage",
		"scripts/run.sh":  "echo hi",
	})

	bundle, supported, err := loadRuntimeLocalSkillBundle("native", "shared-skill")
	if err != nil {
		t.Fatalf("loadRuntimeLocalSkillBundle: %v", err)
	}
	if !supported {
		t.Fatal("native should be supported")
	}
	if bundle.Name != "Shared Skill" {
		t.Fatalf("name = %q, want Shared Skill", bundle.Name)
	}
	if len(bundle.Files) != 2 {
		t.Fatalf("expected 2 supporting files, got %d", len(bundle.Files))
	}
	if bundle.SourcePath != "~/.agents/skills/shared-skill" {
		t.Fatalf("source_path = %q", bundle.SourcePath)
	}
}

// Both roots missing → empty list, no error.
func TestListRuntimeLocalSkills_BothRootsMissing(t *testing.T) {
	home := t.TempDir()
	redirectTestHome(t, home)

	skills, supported, err := listRuntimeLocalSkills("native")
	if err != nil {
		t.Fatalf("listRuntimeLocalSkills: %v", err)
	}
	if !supported {
		t.Fatal("native should be supported")
	}
	if len(skills) != 0 {
		t.Fatalf("expected empty, got %v", skills)
	}
}

// Nested layouts (a skill two levels deep) work in the universal root too.
func TestListRuntimeLocalSkills_NestedSkillInUniversalRoot(t *testing.T) {
	home := t.TempDir()
	redirectTestHome(t, home)

	writeTestLocalSkill(t, filepath.Join(home, ".agents", "skills"), "release/reporter", map[string]string{
		"SKILL.md": "---\nname: Release Reporter\n---\n",
	})

	skills, _, err := listRuntimeLocalSkills("native")
	if err != nil {
		t.Fatalf("listRuntimeLocalSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Key != "release/reporter" {
		t.Fatalf("expected release/reporter, got %v", skills)
	}
	if skills[0].Root != localSkillRootUniversal {
		t.Fatalf("root = %q, want %q", skills[0].Root, localSkillRootUniversal)
	}

	bundle, _, err := loadRuntimeLocalSkillBundle("native", "release/reporter")
	if err != nil {
		t.Fatalf("loadRuntimeLocalSkillBundle: %v", err)
	}
	if bundle.Name != "Release Reporter" {
		t.Fatalf("bundle name = %q", bundle.Name)
	}
}

// invalidUTF8Docx is a .docx prefix (PK zip magic) followed by bytes that are
// not valid UTF-8. Carrying this through SkillFileData.Content is exactly the
// corruption in #7143: encoding/json rewrites each invalid byte to U+FFFD, so
// the file written back into the task environment no longer opens.
const invalidUTF8Docx = "PK\x03\x04\x14\x00\x06\x00\xff\xfe\x80\x81payload"

func TestCollectLocalSkillFiles_SkipsBinarySupportingFiles(t *testing.T) {
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":               "---\nname: docs\n---\nbody\n",
		"references/notes.md":    "# notes\n",
		"references/report.docx": invalidUTF8Docx,
		"assets/logo.png":        "\x89PNG\r\n\x1a\n\x00\x00\x00",
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("collected paths = %v, want %v", paths, want)
	}

	// The text file that survives must survive byte-for-byte.
	if files[0].Content != "# notes\n" {
		t.Errorf("text content = %q, want %q", files[0].Content, "# notes\n")
	}
}

func TestCollectLocalSkillFiles_BinarySkipIsConsistentAcrossPasses(t *testing.T) {
	// Discovery reports the bundle with includeContent=false and sync uploads
	// it with includeContent=true. If only one pass skipped binary files the
	// two would disagree on what the bundle contains. This must hold both for
	// a listed binary extension and for one caught only by the content-level
	// UTF-8 check — the discovery pass has to actually read the file to make
	// the same call the sync pass makes.
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":                  "---\nname: docs\n---\nbody\n",
		"references/notes.md":       "# notes\n",
		"references/report.docx":    invalidUTF8Docx,
		"weights/model.safetensors": invalidUTF8Bytes,
	})

	listed, err := collectLocalSkillFiles(skillDir, false)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles(includeContent=false): %v", err)
	}
	synced, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles(includeContent=true): %v", err)
	}

	listedPaths := make([]string, 0, len(listed))
	for _, f := range listed {
		listedPaths = append(listedPaths, f.Path)
	}
	syncedPaths := make([]string, 0, len(synced))
	for _, f := range synced {
		syncedPaths = append(syncedPaths, f.Path)
	}
	if !reflect.DeepEqual(listedPaths, syncedPaths) {
		t.Fatalf("discovery pass = %v, sync pass = %v; passes must agree", listedPaths, syncedPaths)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(listedPaths, want) {
		t.Fatalf("collected paths = %v, want %v", listedPaths, want)
	}
}

// invalidUTF8Bytes carries no listed binary extension's magic — it exists to
// prove the content-level check, not the extension list, is what catches it.
const invalidUTF8Bytes = "\xff\xfe\x00\x01payload\x80\x81"

// Bohan-J's review on #7175: the extension blacklist alone doesn't close the
// root cause. A binary file with an unlisted extension must still be caught
// by the content-level utf8.Valid check.
func TestCollectLocalSkillFiles_SkipsUnlistedBinaryExtension(t *testing.T) {
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":                  "---\nname: docs\n---\nbody\n",
		"references/notes.md":       "# notes\n",
		"weights/model.safetensors": invalidUTF8Bytes,
		"data/table.parquet":        invalidUTF8Bytes,
		"blob.bin":                  invalidUTF8Bytes,
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("collected paths = %v, want %v — an unlisted binary extension must still be caught by the content check", paths, want)
	}
}

// A binary file with no extension at all must be caught the same way.
func TestCollectLocalSkillFiles_SkipsInvalidUTF8WithNoExtension(t *testing.T) {
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":            "---\nname: docs\n---\nbody\n",
		"references/notes.md": "# notes\n",
		"references/README":   invalidUTF8Bytes,
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("collected paths = %v, want %v — a no-extension binary file must still be caught by the content check", paths, want)
	}
}

// validUTF8WithNUL is UTF-16LE encoding of "Hello": every byte is a valid
// single-byte UTF-8 sequence (printable ASCII or NUL), so utf8.Valid alone
// would accept it — multica-eve's review on #7175 flagged this as a
// realistic byte-integrity hole: the server-side import path
// (server/internal/handler/skill_create.go's sanitizeNullBytes) strips every
// 0x00, so a file like this still comes back different from what went in.
const validUTF8WithNUL = "H\x00e\x00l\x00l\x00o\x00"

// A file with an unlisted extension that is valid UTF-8 but contains an
// embedded NUL must still be caught — utf8.Valid alone is not sufficient.
func TestCollectLocalSkillFiles_SkipsUnlistedExtensionWithEmbeddedNUL(t *testing.T) {
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":            "---\nname: docs\n---\nbody\n",
		"references/notes.md": "# notes\n",
		"weights/model.dat":   validUTF8WithNUL,
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("collected paths = %v, want %v — valid UTF-8 with an embedded NUL must still be caught", paths, want)
	}
}

// A no-extension file with the same valid-UTF-8-but-NUL-containing content
// must be caught the same way.
func TestCollectLocalSkillFiles_SkipsNoExtensionWithEmbeddedNUL(t *testing.T) {
	root := t.TempDir()
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":             "---\nname: docs\n---\nbody\n",
		"references/notes.md":  "# notes\n",
		"references/UTF16NAME": validUTF8WithNUL,
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"references/notes.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("collected paths = %v, want %v — a no-extension file with an embedded NUL must still be caught", paths, want)
	}
}

// A valid-UTF-8 text file — including one with an extension the blacklist
// doesn't recognize, and one with no extension at all — must still pass
// through untouched. The content check must not over-trigger on ordinary
// text.
func TestCollectLocalSkillFiles_ValidUTF8TextPassesThroughUntouched(t *testing.T) {
	root := t.TempDir()
	body := "# Notes\n\nContém acentos e emoji 🎉 — não é binário.\n"
	skillDir := writeTestLocalSkill(t, root, "docs", map[string]string{
		"SKILL.md":  "---\nname: docs\n---\nbody\n",
		"notes.txt": body,
		"data.csv":  "a,b,c\n1,2,3\n",
		"config":    "key=value\n",
	})

	files, err := collectLocalSkillFiles(skillDir, true)
	if err != nil {
		t.Fatalf("collectLocalSkillFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected all 3 valid-UTF-8 supporting files to pass through, got %d: %+v", len(files), files)
	}
	for _, f := range files {
		if f.Path == "notes.txt" && f.Content != body {
			t.Errorf("notes.txt content = %q, want %q", f.Content, body)
		}
	}
}
