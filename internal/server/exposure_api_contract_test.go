package server

import "hy2tun/internal/webui/serverui"

// Compile-time guard: the Web console's optional exposure interface must stay
// implemented by the production server. Without this, a signature drift would
// silently turn the endpoint into a 501 while the serverui fake tests pass.
var _ serverui.ExposureAPI = (*Server)(nil)
