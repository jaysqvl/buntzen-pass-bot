// Package buildinfo identifies the source embedded in the running binary.
package buildinfo

// Version and Revision are set by the release image build with Go linker flags.
// Native builds without those flags remain explicitly development builds.
var (
	Version  = "dev"
	Revision = ""
)
