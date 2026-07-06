package uowstore

import (
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
)

// SpikeBackend is the Backend value carried by every typed-unsupported error
// from this package. Pinned by tests; do not change without updating the
// error-text contract in the spike docs.
const SpikeBackend = "uowstore spike"

// errUnsupported builds the typed error every generated stub returns. The seam
// type (*storage.ErrUnsupported) stays rendering-free; the spike-context wrap
// (BD_SPIKE_UOWSTORE / issue #4547) lives only here.
func errUnsupported(op string) error {
	return fmt.Errorf("%w (BD_SPIKE_UOWSTORE spike shell, gastownhall/beads#4547)",
		&storage.ErrUnsupported{Op: op, Backend: SpikeBackend})
}
