package engine

import "errors"

// ErrSOAPUnsupported reports that the operation is a SOAP call, which Engine no longer executes.
var ErrSOAPUnsupported = errors.New("SOAP operations are not supported by this Engine")
