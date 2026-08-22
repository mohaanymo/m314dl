//go:build !linux

package worker

// Off Linux there is no /proc to read. The worker still serves, and the panel
// shows the same blanks it showed before any of this was reported.
func sampleSysStat() sysStat { return sysStat{} }
