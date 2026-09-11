package server

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
)

// TestRestatedErrorKeepsItsSentinel is the guard for a trap this code fell into
// once already: stopRemote presents its own sentence for an unfinished Claude
// turn, and the status code used to be recovered by string-matching that
// sentence. Rewording it silently turned every 409 into a 400 and the dashboard
// stopped offering a forced retry. The sentinel now travels with the error.
func TestRestatedErrorKeepsItsSentinel(t *testing.T) {
	t.Parallel()
	err := restate("this session has an unfinished Claude turn; confirm a forced stop", claude.ErrStopTimeout)

	if got := err.Error(); got != "this session has an unfinished Claude turn; confirm a forced stop" {
		t.Fatalf("message = %q, want the restated sentence", got)
	}
	if !errors.Is(err, claude.ErrStopTimeout) {
		t.Fatal("restated error lost the ErrStopTimeout sentinel")
	}
	if got := stopFailureStatus(err); got != http.StatusConflict {
		t.Fatalf("stopFailureStatus = %d, want 409", got)
	}
	if !forcible(failStop(err)) {
		t.Fatal("a stop conflict must be reported as forcible so the dashboard can offer a forced retry")
	}
}

func TestStatusOfMapsOperationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, http.StatusOK},
		{"explicit status is kept", failText(http.StatusNotFound, "missing"), http.StatusNotFound},
		{"stop conflict is a 409", failStop(restate("busy", claude.ErrStopTimeout)), http.StatusConflict},
		{"start conflict is a 409", failStart(claude.ErrStopTimeout), http.StatusConflict},
		{"other start failures are bad requests", failStart(errors.New("nope")), http.StatusBadRequest},
		{"an unwrapped error is a server fault", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := statusOf(testCase.err); got != testCase.want {
				t.Fatalf("statusOf = %d, want %d", got, testCase.want)
			}
		})
	}
}

// TestErrorMessageRedaction pins the disclosure rules shared by the JSON API and
// the rendered dashboard, so neither surface can start leaking more than the other.
func TestErrorMessageRedaction(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}

	if got := errorMessage(errors.New("stack trace"), http.StatusInternalServerError); got == "stack trace" {
		t.Fatal("a 5xx must not disclose its error text")
	}
	if got := errorMessage(errors.New("cannot read "+home+"/secret"), http.StatusBadRequest); got != "cannot read ~/secret" {
		t.Fatalf("home directory not redacted: %q", got)
	}
	if got := errorMessage(errors.New("one\n\ttwo   three"), http.StatusBadRequest); got != "one two three" {
		t.Fatalf("whitespace not folded: %q", got)
	}
	long := make([]byte, 400)
	for index := range long {
		long[index] = 'x'
	}
	if got := errorMessage(errors.New(string(long)), http.StatusBadRequest); len([]rune(got)) != 240 {
		t.Fatalf("message length = %d runes, want 240", len([]rune(got)))
	}
	if got := errorMessage(errors.New("   "), http.StatusBadRequest); got != "Request failed." {
		t.Fatalf("empty message = %q, want a fallback", got)
	}
}
