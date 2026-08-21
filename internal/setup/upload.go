package setup

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jphenow/sp/internal/sprite"
)

// uploadAttempts is how many times UploadFiles tries a push before giving up.
// The sprite fs/write API regularly blows its (non-configurable) HTTP client
// timeout on a cold sprite — "request canceled (Client.Timeout exceeded while
// awaiting headers)" — and the sprite is usually warm by the second try.
const uploadAttempts = 2

// UploadFiles pushes local→remote file pairs to a sprite in a SINGLE exec.
// `sprite exec --file` is repeatable, so batching N files into one call costs
// one round trip instead of N — which matters a lot here, because each exec
// against a cold sprite has been observed to take 30s+ and the whole point is
// to stay under the CLI's upload timeout.
//
// Retries the whole batch on failure (see uploadAttempts). Returns the last
// error; callers decide whether a missing dotfile is fatal (it generally
// isn't) but MUST NOT discard the error silently — a swallowed timeout here
// is indistinguishable from success and leaves the user staring at a sprite
// with none of their config on it.
func UploadFiles(client *sprite.Client, spriteName string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	if err := uploadBatch(client, spriteName, files); err == nil {
		return nil
	}
	if len(files) == 1 {
		// Nothing to salvage — uploadBatch already retried.
		return uploadBatch(client, spriteName, files)
	}
	// The CLI uploads a batch serially and aborts on the first failure, so one
	// bad destination takes the rest of the set with it. Fall back to one
	// upload per file to get partial success and to name the file that's
	// actually broken instead of blaming the whole batch.
	var failed []string
	var last error
	for local, remote := range files {
		if err := uploadBatch(client, spriteName, map[string]string{local: remote}); err != nil {
			failed = append(failed, local)
			last = err
		}
	}
	if len(failed) == 0 {
		return nil
	}
	sort.Strings(failed)
	return fmt.Errorf("uploading to sprite %q failed for %s: %w", spriteName, strings.Join(failed, ", "), last)
}

// uploadBatch runs one `sprite exec --file ...` upload, retrying the whole set.
func uploadBatch(client *sprite.Client, spriteName string, files map[string]string) error {
	var err error
	for attempt := 0; attempt < uploadAttempts; attempt++ {
		_, err = client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"true"},
			Files:   files,
		})
		if err == nil {
			return nil
		}
	}
	return err
}
