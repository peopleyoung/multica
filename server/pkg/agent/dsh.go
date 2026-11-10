package agent

import (
	"time"
)

const (
	dshProfile = "multica"
	// DshProtocolVersion is the DSH stdio protocol this backend speaks. Exported
	// because the daemon's profile probe accepts or rejects a runtime profile on
	// this exact number, and a second copy of it there is a copy that can drift.
	DshProtocolVersion = 1
	dshCancelGrace     = 3 * time.Second
	dshTerminateGrace  = 2 * time.Second
)
