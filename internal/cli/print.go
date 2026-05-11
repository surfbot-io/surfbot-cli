package cli

import (
	"fmt"
	"io"
)

// pf and pln write to w and ignore the error. Terminal writes don't fail
// in any actionable way, and the errcheck pattern around fmt.Fprintln
// produces noise in every command. Centralizing the swallow keeps the
// linter happy and the call sites readable.
func pf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func pln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }
