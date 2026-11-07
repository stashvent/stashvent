package security

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stashvent/stashvent/apikey"
)

// The fake credentials below are put together at run time, so this file holds
// no secret-shaped text: secret scanners (including ours, see
// TestFixturesAreNotSecrets) and push protection leave the repository alone.
// Well-known sample keys can't be used instead; gitleaks allowlists them.
const (
	filler32 = "x8Kq2ZpL9vTn4WbR7sYd3FhJ6mNc1GaE"
	filler16 = "Q3EGUTZ7VLKM2XPD"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func awsKey() string    { return "AKIA" + filler16 }
func stripeKey() string { return "sk_" + "live_" + filler32 }
func jwt() string {
	return b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"sub":"1234567890","name":"dev"}`) + "." + filler32
}
func privateKey() []string {
	return []string{
		"-----BEGIN RSA PRIVATE " + "KEY-----",
		filler32 + filler32,
		filler32 + filler32,
		"-----END RSA PRIVATE " + "KEY-----",
	}
}

// newFile is the patch `git diff` prints for a new file with these lines.
func newFile(path string, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%[1]s b/%[1]s\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/%[1]s\n@@ -0,0 +1,%d @@\n",
		path, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// scan scans patch as a stash's working tree, with nothing staged.
func scan(t *testing.T, patch string) []Finding {
	t.Helper()
	return scanStash(t, patch, "")
}

func scanStash(t *testing.T, worktree, index string) []Finding {
	t.Helper()
	s, err := NewScanner()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.Scan(t.Context(), worktree, index)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return findings
}

// gitleaks loads its rules through process-wide state, so two scanners made
// at the same moment used to race. Under -race this fails without the lock
// in NewScanner; each scanner must also still work.
func TestScannersCanBeMadeConcurrently(t *testing.T) {
	patch := newFile(".env", "AWS_ACCESS_KEY_ID="+awsKey())
	const scanners = 4
	found := make([]int, scanners)
	errs := make([]error, scanners)
	var wg sync.WaitGroup
	for i := range scanners {
		wg.Go(func() {
			s, err := NewScanner()
			if err != nil {
				errs[i] = err
				return
			}
			findings, err := s.Scan(t.Context(), patch, "")
			found[i], errs[i] = len(findings), err
		})
	}
	wg.Wait()
	for i := range scanners {
		if errs[i] != nil || found[i] != 1 {
			t.Errorf("scanner %d: %d findings, %v; want 1", i, found[i], errs[i])
		}
	}
}

// One case per kind of secret the scan must find, each pinned to its rule ID,
// so a gitleaks upgrade that renames or drops one fails here.
func TestScanFindsEachKindOfSecret(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		rule  string
		line  int
	}{
		{"AWS access key", []string{"AWS_ACCESS_KEY_ID=" + awsKey()}, "aws-access-token", 1},
		{"Stripe key", []string{"# billing", "STRIPE_SECRET_KEY=" + stripeKey()}, "stripe-access-token", 2},
		{"private key", privateKey(), "private-key", 1},
		{"JWT", []string{"Authorization: Bearer " + jwt()}, "jwt", 1},
		{".env value", []string{"PORT=8080", "", "SESSION_SECRET=" + filler32}, "generic-api-key", 3},
		{"API key in code", []string{`const apiKey = "` + filler32 + `"`}, "generic-api-key", 1},
		{"database password", []string{"DB_PASSWORD=" + filler32}, "generic-api-key", 1},
		{"cloud credential (GitHub token)", []string{"GITHUB_TOKEN=" + "ghp_" + filler32 + "aB3d"}, "github-pat", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scan(t, newFile("settings.txt", tt.lines...))
			want := Finding{File: "settings.txt", Line: tt.line, RuleID: tt.rule}
			if !slices.Contains(got, want) {
				t.Errorf("findings = %+v, want %+v among them", got, want)
			}
		})
	}
}

// StashVent's own keys are found by a rule of ours, wherever they sit.
func TestScanFindsStashVentAPIKey(t *testing.T) {
	key, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"environment file":  "STASHVENT_API_KEY=" + key,
		"quoted in code":    `client.Login("` + key + `")`,
		"shell profile":     "export STASHVENT_API_KEY='" + key + "'",
		"pasted on its own": key,
	} {
		t.Run(name, func(t *testing.T) {
			got := scan(t, newFile("notes.txt", "first line", line))
			want := Finding{File: "notes.txt", Line: 2, RuleID: RuleStashVentAPIKey}
			if !slices.Contains(got, want) {
				t.Errorf("findings = %+v, want %+v among them", got, want)
			}
		})
	}

	// Not a key: too short, or part of a longer word.
	for _, line := range []string{
		"sv_" + filler16,
		"prefix_sv_" + filler32 + filler16 + "x",
	} {
		for _, f := range scan(t, newFile("notes.txt", line)) {
			if f.RuleID == RuleStashVentAPIKey {
				t.Errorf("%q reported as a StashVent key", line)
			}
		}
	}
}

// The gitleaks default rules miss a password inside a URL; a rule of ours
// finds it, but not a placeholder or a URL that has no password.
func TestScanFindsPasswordInURL(t *testing.T) {
	for name, line := range map[string]string{
		"postgres":               "DATABASE_URL=postgres://app:" + filler16 + "@db.internal:5432/app",
		"mysql in code":          `dsn := "mysql://root:` + filler16 + `@127.0.0.1:3306/shop"`,
		"redis":                  "REDIS_URL=redis://default:" + filler16 + "@cache:6379/0",
		"amqp":                   "amqp://guest:" + filler16 + "@rabbit/vhost",
		"https with a git token": "git remote add origin https://dev:" + filler32 + "@git.example.com/team/app.git",
		"password with a colon":  "postgres://app:" + filler16 + ":x@db/app",
		"short dev password":     "postgres://stashvent:" + "stashvent@localhost:5438/stashvent",
	} {
		t.Run(name, func(t *testing.T) {
			got := scan(t, newFile("settings.txt", "first line", line))
			want := Finding{File: "settings.txt", Line: 2, RuleID: RuleURLPassword}
			if !slices.Contains(got, want) {
				t.Errorf("findings = %+v, want %+v among them", got, want)
			}
		})
	}

	for name, line := range map[string]string{ //nolint:gosec // G101: placeholders and URLs without a password, not credentials
		"shell variable":       "DATABASE_URL=postgres://app:${DB_PASSWORD}@db/app",
		"bare variable":        "postgres://app:$DB_PASSWORD@db/app",
		"Windows variable":     "postgres://app:%DB_PASSWORD%@db/app",
		"template":             "postgres://app:{{.Password}}@db/app",
		"Make variable":        "postgres://app:$(DB_PASSWORD)@db/app",
		"Python f-string":      `url = f"postgresql://{user}:{password}@{host}/{db}"`,
		"Python % formatting":  `"mysql://%(user)s:%(password)s@%(host)s/shop" % cfg`,
		"image by digest":      "uses: docker://alpine:3.20@sha256:ab12" + strings.Repeat("ab12", 15),
		"angle brackets":       "postgres://user:<password>@host/db",
		"mask":                 "postgres://app:****@db/app",
		"word":                 "postgres://app:password@db/app",
		"no password":          "ssh://git@github.com:22/team/app.git",
		"user only":            "https://dev@git.example.com/team/app.git",
		"port, not a password": "http://localhost:8080/v1/shares",
	} {
		t.Run("not "+name, func(t *testing.T) {
			for _, f := range scan(t, newFile("settings.txt", line)) {
				if f.RuleID == RuleURLPassword {
					t.Errorf("%q reported as a password in a URL", line)
				}
			}
		})
	}
}

func TestScanReportsFileAndLine(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/config/app.conf b/config/app.conf",
		"index 1111111..2222222 100644",
		"--- a/config/app.conf",
		"+++ b/config/app.conf",
		"@@ -4 +4 @@",
		"-name = old",
		"+name = new",
		"@@ -10,0 +11,2 @@",
		"+region = eu-west-1",
		"+aws_access_key_id = " + awsKey(),
		"",
	}, "\n")
	got := scan(t, patch)
	want := []Finding{{File: "config/app.conf", Line: 12, RuleID: "aws-access-token"}}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}

func TestScanIgnoresRemovedLines(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/app.conf b/app.conf",
		"index 1111111..2222222 100644",
		"--- a/app.conf",
		"+++ b/app.conf",
		"@@ -3 +3 @@",
		"-aws_access_key_id = " + awsKey(),
		"+aws_access_key_id = ${AWS_ACCESS_KEY_ID}",
		"",
	}, "\n")
	if got := scan(t, patch); len(got) != 0 {
		t.Errorf("findings = %+v, want none: the secret is on a removed line", got)
	}
}

// The index usually repeats the working tree. What both have is reported
// once; only what the index alone has is marked staged.
func TestScanSortsAndMarksStagedOnly(t *testing.T) {
	zeta := newFile("zeta.env", "KEY_ID="+awsKey())
	alpha := newFile("alpha.env", "# keys", "STRIPE="+stripeKey(), "KEY_ID="+awsKey())
	alphaStaged := newFile("alpha.env", "# keys", "STRIPE="+stripeKey()) // an earlier version of the same file
	undone := newFile("undone.conf", "KEY_ID="+awsKey())                 // staged, then removed from the working tree

	got := scanStash(t, zeta+alpha, zeta+alphaStaged+undone)
	want := []Finding{
		{File: "alpha.env", Line: 2, RuleID: "stripe-access-token"},
		{File: "alpha.env", Line: 3, RuleID: "aws-access-token"},
		{File: "undone.conf", Line: 1, RuleID: "aws-access-token", Staged: true},
		{File: "zeta.env", Line: 1, RuleID: "aws-access-token"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}

// After `git add -A && git stash` the index patch is the working tree patch
// again; it must not cost a second pass through the rules.
func TestScanSkipsHunksAlreadyScanned(t *testing.T) {
	patch := newFile("a.env", "KEY_ID="+awsKey()) + newFile("main.go", "package main")
	scanned := func(index string) uint64 {
		s, err := NewScanner()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Scan(t.Context(), patch, index); err != nil {
			t.Fatal(err)
		}
		return s.detector.TotalBytes.Load()
	}
	if once, twice := scanned(""), scanned(patch); twice != once {
		t.Errorf("scanned %d bytes with the same patch as index, want %d as without", twice, once)
	}
}

// With `git diff --text`, a UTF-16 file (what Windows PowerShell's `>`
// writes) arrives as lines with a zero byte after every character.
func TestScanReadsUTF16(t *testing.T) {
	var content strings.Builder
	content.WriteString("\xff\xfe") // byte order mark
	for _, c := range []byte("PORT=8080\r\nKEY_ID=" + awsKey() + "\r\n") {
		content.WriteByte(c)
		content.WriteByte(0)
	}
	got := scan(t, newFile("keys.txt", strings.Split(content.String(), "\n")...))
	want := []Finding{{File: "keys.txt", Line: 2, RuleID: "aws-access-token"}}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}

func TestScanSkipsFilesWithoutText(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/logo.png b/logo.png",
		"new file mode 100644",
		"index 0000000..1111111",
		"Binary files /dev/null and b/logo.png differ",
		"diff --git a/run.sh b/run.sh",
		"old mode 100644",
		"new mode 100755",
		"diff --git a/old.txt b/old.txt",
		"deleted file mode 100644",
		"index 1111111..0000000",
		"--- a/old.txt",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-gone",
		"",
	}, "\n") + newFile("last.env", "KEY_ID="+awsKey())
	got := scan(t, patch)
	want := []Finding{{File: "last.env", Line: 1, RuleID: "aws-access-token"}}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}

func TestScanDecodesQuotedPath(t *testing.T) {
	// Git quotes paths with special characters: "a/caf\303\251 keys.env".
	quoted := `caf\303\251 keys.env`
	patch := strings.Join([]string{
		`diff --git "a/` + quoted + `" "b/` + quoted + `"`,
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		`+++ "b/` + quoted + `"`,
		"@@ -0,0 +1 @@",
		"+KEY_ID=" + awsKey(),
		"",
	}, "\n")
	got := scan(t, patch)
	want := []Finding{{File: "café keys.env", Line: 1, RuleID: "aws-access-token"}}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}

func TestScanHonoursGitleaksAllow(t *testing.T) {
	got := scan(t, newFile("fixtures.env", "KEY_ID="+awsKey()+" # gitleaks:allow"))
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none: the line is marked gitleaks:allow", got)
	}
}

func TestScanCleanPatch(t *testing.T) {
	for name, patch := range map[string]string{
		"empty":      "",
		"no secrets": newFile("main.go", "package main", "", "func main() {}"),
	} {
		if got := scan(t, patch); len(got) != 0 {
			t.Errorf("%s: findings = %+v, want none", name, got)
		}
	}
}

// A patch the parser gives up on must be an error, not a scan that quietly
// skipped the rest.
func TestScanUnreadablePatch(t *testing.T) {
	broken := strings.Join([]string{
		"diff --git a/a.txt b/a.txt",
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/a.txt",
		"@@ -0,0 +1,5 @@", // says five lines, has one
		"+only one",
		"",
	}, "\n") + newFile("b.env", "KEY_ID="+awsKey())

	s, err := NewScanner()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Scan(t.Context(), "", broken)
	if !errors.Is(err, ErrUnreadablePatch) {
		t.Errorf("Scan = %+v, %v; want ErrUnreadablePatch", got, err)
	}
}

func TestScanCancelled(t *testing.T) {
	s, err := NewScanner()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Scan(ctx, newFile("a.env", "KEY_ID="+awsKey()), ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan error = %v, want context.Canceled", err)
	}
}

func TestFixturesAreNotSecrets(t *testing.T) {
	src, err := os.ReadFile("scan_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := scan(t, newFile("scan_test.go", strings.Split(string(src), "\n")...)); len(got) != 0 {
		t.Errorf("this test file trips the scanner: %+v", got)
	}
}
