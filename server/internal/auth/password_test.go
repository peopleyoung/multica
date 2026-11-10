package auth

import "testing"

func TestPasswordCredentials(t *testing.T) {
	for _, value := range []string{"ab", "name@example.com", "_name", "bad name"} {
		if _, err := NormalizeUsername(value); err == nil {
			t.Fatalf("accepted username %q", value)
		}
	}
	if got, err := NormalizeUsername(" Alice.Test "); err != nil || got != "alice.test" {
		t.Fatalf("normalize=%q %v", got, err)
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("accepted short password")
	}
	hash, err := HashPassword("Strong password 2026")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "Strong password 2026" || !CheckPassword(hash, "Strong password 2026") || CheckPassword(hash, "wrong") || CheckPassword("", "unusable-account-placeholder") {
		t.Fatal("incorrect password comparison")
	}
}
