module("luci.controller.hy2tun", package.seeall)

local fs = require "nixio.fs"

function index()
	if not fs.access("/etc/init.d/hy2tun") then
		return
	end

	entry({"admin", "services", "hy2tun"}, template("hy2tun/status"), _("hy2tun"), 60).dependent = false
end
