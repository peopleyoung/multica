package nativeruntime

import "testing"

func TestPublicLimitsNeverPublishCredentialsOrQueryStrings(t *testing.T) {
	limits := Limits{HTTPOrigins: []string{"http://user:private@example.test", "https://cvat.example/api?token=private", "https://cvat.example"}}.Public()
	if len(limits.HTTPOrigins) != 1 || limits.HTTPOrigins[0] != "https://cvat.example" {
		t.Fatal("unsafe origins entered public host metadata")
	}
}

func TestConfigRejectsCredentialsAndUnsupportedOptions(t *testing.T) {
	for _, raw := range []string{`{"command":"sh"}`, `[]`, `false`, `{"services":[{"name":"cvat","base_url":"http://user:secret@example.test"}]}`, `{"services":[{"name":"cvat","base_url":"http://example.test/?token=secret"}]}`, `{"services":[{"name":"cvat","base_url":"http://example.test","token":"secret"}]}`, `{"services":[{"name":"cvat","base_url":"http://example.test","token_env":"MULTICA_TOKEN"}]}`, `{"services":[{"name":"cvat","base_url":"http://example.test","token_env":"CVAT_TOKEN","auth_scheme":"secret"}]}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("invalid configuration accepted: %s", raw)
		}
	}
}

func TestConfigCannotWidenHostPermissions(t *testing.T) {
	cfg, err := Parse([]byte(`{"write_files":true,"shell":true,"services":[{"name":"cvat","base_url":"https://cvat.example/api","token_env":"CVAT_TOKEN","auth_scheme":"Token"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, limits := range []Limits{{}, {WriteFiles: true}, {WriteFiles: true, Shell: true}, {WriteFiles: true, Shell: true, HTTPOrigins: []string{"https://other.example"}}} {
		if cfg.Check(limits) == nil {
			t.Fatal("agent widened host permissions")
		}
	}
	if err := cfg.Check(Limits{WriteFiles: true, Shell: true, HTTPOrigins: []string{"https://cvat.example"}}); err != nil {
		t.Fatal(err)
	}
	empty, err := Parse([]byte(`{}`))
	if err != nil || empty.WriteFiles || empty.Shell {
		t.Fatal("missing settings granted host authority")
	}
}

func TestRemoteEnvironmentRequiresExplicitCompatibleService(t *testing.T) {
	for _, raw := range []string{`{"environment":"ssh"}`, `{"environment":"multica_jobs"}`, `{"environment":"multica_jobs","shell":true,"services":[{"name":"multica_jobs","base_url":"http://localhost:23110","allow_write":false}]}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted incompatible execution environment: %s", raw)
		}
	}
	cfg, err := Parse([]byte(`{"environment":"multica_jobs","shell":true,"services":[{"name":"multica_jobs","base_url":"http://localhost:23110","allow_write":true}]}`))
	if err != nil || cfg.Environment != "multica_jobs" {
		t.Fatal(err)
	}
}
