package cli

import "errors"

// execSelf is not possible on Windows. The daemon does not ask for it
// there: the new binary runs from the next start.
func execSelf(string) error { return errors.New("windows cannot replace a running process") }
