package security

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/gitleaks/go-gitdiff/gitdiff"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	"github.com/zricethezav/gitleaks/v8/regexp"

	"github.com/stashvent/stashvent/apikey"
)

// Finding is one likely secret in a patch. It deliberately has no field for
// the secret or the line it is on, so callers can't print or log either.
type Finding struct {
	File   string // path in the repository, "/"-separated
	Line   int    // line number in the new version of the file
	RuleID string // gitleaks rule that matched, e.g. "aws-access-token"
	// Staged means the secret is only in the staged version of the file,
	// which `git stash show` doesn't print.
	Staged bool
}

// ErrUnreadablePatch means the patch couldn't be parsed completely, so a
// scan of it would have skipped files.
var ErrUnreadablePatch = errors.New("stash diff could not be parsed")

// Scanner finds likely secrets with the gitleaks default rules.
type Scanner struct {
	detector *detect.Detector
}

var (
	silenceLogger sync.Once
	// loadRules lets one NewScanner at a time into gitleaks: it reads its
	// default rules through viper's process-wide state.
	loadRules sync.Mutex
)

// RuleStashVentAPIKey is the rule that finds a StashVent API key. gitleaks
// doesn't know the format, and a key in a stash would hand the sender's
// identity to every recipient.
const RuleStashVentAPIKey = "stashvent-api-key" //nolint:gosec // G101: a rule ID, not a credential

// RuleURLPassword is the rule that finds a password inside a URL, such as a
// database connection string. The default rules only catch a password that
// sits in a variable of its own (DB_PASSWORD=…).
const RuleURLPassword = "stashvent-url-password" //nolint:gosec // G101: a rule ID, not a credential

// urlPassword matches scheme://user:password@host, with the password as
// group 1. The user stops at the first ':'; neither part may hold '@', '/',
// or whitespace, so a URL with only a user (ssh://git@host:22/) can't match.
// '<' and '>' are left out too: "<password>" is documentation, not a value.
const urlPassword = `\b[A-Za-z][A-Za-z0-9+.\-]*://[^\s:/?#@'"<>]+:([^\s/?#@'"<>]{3,})@[^\s/?#@'"<>]+` //nolint:gosec // G101: the pattern that finds credentials, not one

// urlPasswordPlaceholders are passwords that only stand in for one: a
// variable to be filled in, a mask, or a word that says "put it here".
var urlPasswordPlaceholders = []string{
	`^\$\{[^}]*\}$`,              // ${DB_PASSWORD}
	`^\$\([^)]*\)$`,              // $(DB_PASSWORD), Makefiles and Kubernetes
	`^\$[A-Za-z_][A-Za-z0-9_]*$`, // $DB_PASSWORD
	`^%[A-Za-z_][A-Za-z0-9_]*%$`, // %DB_PASSWORD%
	`^%\([^)]*\)s$`,              // %(password)s, Python's % formatting
	`^\{+[^{}]*\}+$`,             // {password} (f-strings, format), {{.Password}}
	`^\*+$`,                      // ***
	`(?i)^x+$`,                   // xxx
	`(?i)^(password|passwd|pass|pwd|secret|changeme)$`,
}

// urlPasswordDigest matches a "host" that is a content digest: an image
// pinned by digest (docker://alpine:3.20@sha256:…) has the shape of
// user:password@host, but holds no password.
const urlPasswordDigest = `@sha(256|384|512):[0-9a-fA-F]`

// NewScanner loads the rules built into the pinned gitleaks version, plus
// RuleStashVentAPIKey and RuleURLPassword. It is safe to call from several
// goroutines.
func NewScanner() (*Scanner, error) {
	// gitleaks logs to stderr in its own format; the CLI reports problems itself.
	silenceLogger.Do(func() { logging.Logger = zerolog.Nop() })

	cfg, err := defaultRules()
	if err != nil {
		return nil, fmt.Errorf("loading secret scan rules: %w", err)
	}
	placeholders := make([]*regexp.Regexp, 0, len(urlPasswordPlaceholders))
	for _, p := range urlPasswordPlaceholders {
		placeholders = append(placeholders, regexp.MustCompile(p))
	}
	rules := []config.Rule{
		{
			RuleID:      RuleStashVentAPIKey,
			Description: "StashVent API key",
			Regex:       regexp.MustCompile(apikey.Pattern),
			Keywords:    []string{strings.ToLower(apikey.Prefix)},
		},
		{
			RuleID:      RuleURLPassword,
			Description: "Password in a URL",
			Regex:       regexp.MustCompile(urlPassword),
			SecretGroup: 1,
			Keywords:    []string{"://"},
			Allowlists: []*config.Allowlist{
				// Without a RegexTarget, gitleaks tests these against the
				// secret, which is the password.
				{Description: "placeholders", Regexes: placeholders},
				{Description: "digests", RegexTarget: "match", Regexes: []*regexp.Regexp{regexp.MustCompile(urlPasswordDigest)}},
			},
		},
	}
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return nil, fmt.Errorf("loading secret scan rules: %w", err)
		}
		cfg.Rules[rule.RuleID] = rule
		cfg.OrderedRules = append(cfg.OrderedRules, rule.RuleID)
		for _, k := range rule.Keywords {
			cfg.Keywords[k] = struct{}{}
		}
	}
	return &Scanner{detector: detect.NewDetector(cfg)}, nil
}

// defaultRules reads gitleaks' default configuration, as
// detect.NewDetectorDefaultConfig does, so rules can be added to it.
func defaultRules() (config.Config, error) {
	loadRules.Lock()
	defer loadRules.Unlock()
	viper.SetConfigType("toml")
	if err := viper.ReadConfig(strings.NewReader(config.DefaultConfig)); err != nil {
		return config.Config{}, err
	}
	var vc config.ViperConfig
	if err := viper.Unmarshal(&vc); err != nil {
		return config.Config{}, err
	}
	return vc.Translate()
}

// Scan returns the likely secrets on the lines a stash adds, sorted by file
// and line. It takes the two patches of git.StashPatch: the working tree
// (with untracked files) and the index. Removed and context lines are
// ignored: the recipient already has them.
//
// Staged changes usually repeat the working tree's, so a hunk of index that
// is also in worktree is not scanned again, and a finding both have is
// reported once, as the working tree's.
func (s *Scanner) Scan(ctx context.Context, worktree, index string) ([]Finding, error) {
	run := scanRun{
		detector:  s.detector,
		fragments: map[fragmentKey]bool{},
		findings:  map[Finding]bool{},
	}
	if err := run.patch(ctx, worktree, false); err != nil {
		return nil, err
	}
	if err := run.patch(ctx, index, true); err != nil {
		return nil, err
	}

	slices.SortFunc(run.found, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.RuleID, b.RuleID))
	})
	return run.found, nil
}

// scanRun is the state of one Scan across its patches.
type scanRun struct {
	detector  *detect.Detector
	fragments map[fragmentKey]bool // hunks already scanned
	findings  map[Finding]bool     // findings already reported, keyed without Staged
	found     []Finding
}

// fragmentKey identifies the added lines of one hunk.
type fragmentKey struct {
	file string
	line int64
	raw  string
}

func (r *scanRun) patch(ctx context.Context, patch string, staged bool) error {
	// Errors never wrap the parser's: its messages can quote patch lines.
	files, err := gitdiff.Parse(strings.NewReader(patch))
	if err != nil {
		return ErrUnreadablePatch
	}

	parsed := 0
	for file := range files { // always drained, or the parser's goroutine leaks
		parsed++
		for _, tf := range file.TextFragments {
			if tf == nil || ctx.Err() != nil {
				continue
			}
			raw := tf.Raw(gitdiff.OpAdd)
			key := fragmentKey{file: file.NewName, line: tf.NewPosition, raw: raw}
			if r.fragments[key] {
				continue
			}
			r.fragments[key] = true

			// UTF-16 text (what Windows PowerShell's `>` writes) has a zero
			// byte next to every ASCII character, which no rule matches
			// across. Without them it reads as plain text; line breaks, and
			// so line numbers, stay where they are.
			if strings.IndexByte(raw, 0) >= 0 {
				raw = strings.ReplaceAll(raw, "\x00", "")
			}
			// The same fragment gitleaks builds for its own Git scans.
			fragment := detect.Fragment{ //nolint:staticcheck // SA1019: v8's DetectContext takes this type; v9 renames it
				Raw:       raw,
				FilePath:  file.NewName,
				StartLine: int(tf.NewPosition),
			}
			for _, f := range r.detector.DetectContext(ctx, fragment) {
				finding := Finding{File: f.File, Line: f.StartLine, RuleID: f.RuleID}
				if !r.findings[finding] {
					r.findings[finding] = true
					finding.Staged = staged
					r.found = append(r.found, finding)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The parser stops quietly at a file it can't read, which would leave the
	// rest of the patch unscanned. Every file starts with one header line, and
	// no line inside a hunk can start with it, so the counts must match.
	if parsed != fileHeaders(patch) {
		return ErrUnreadablePatch
	}
	return nil
}

// fileHeaders counts the "diff --git" lines of a patch.
func fileHeaders(patch string) int {
	const header = "diff --git "
	n := strings.Count(patch, "\n"+header)
	if strings.HasPrefix(patch, header) {
		n++
	}
	return n
}
