-- Standalone integration harness for the LuCI controller.
--
-- The shell wrapper copies the controller into a temporary directory and
-- rewrites its production paths to local test doubles before this file runs.
-- This harness stubs the LuCI/nixio runtime and drives data()/action() against
-- fake CLI and init-script test doubles. It does not need root, OpenWrt, or
-- the real /etc/hy2tun, /usr/bin/hy2tun, or /etc/init.d/hy2tun paths.

local CONTROLLER = assert(os.getenv("HY2TUN_CONTROLLER"),
	"HY2TUN_CONTROLLER is required; run scripts/openwrt/test-luci-controller.sh")
local TEST_DIR = assert(os.getenv("HY2TUN_TEST_DIR"),
	"HY2TUN_TEST_DIR is required; run scripts/openwrt/test-luci-controller.sh")
local STATE = TEST_DIR .. "/state"

local cjson = require "cjson"

local function shellquote(value)
	return "'" .. tostring(value or ""):gsub("'", "'\\''") .. "'"
end

local function sh(command)
	local status = os.execute(command)
	if status == true then
		return 0
	end
	if type(status) == "number" then
		if status >= 256 then
			return math.floor(status / 256)
		end
		return status
	end
	return 1
end

local function file_exists(path)
	local file = io.open(path, "r")
	if file then
		file:close()
		return true
	end
	return false
end

local function read_file(path)
	local file = io.open(path, "r")
	if not file then
		return nil
	end
	local data = file:read("*a")
	file:close()
	return data
end

local function write_file(path, data)
	local file = assert(io.open(path, "w"))
	assert(file:write(data))
	file:close()
end

local function remove_file(path)
	os.remove(path)
end

assert(sh("mkdir -p " .. shellquote(TEST_DIR) .. " " .. shellquote(STATE)) == 0,
	"unable to create the test directory")

-- The controller creates private temporary files with os.tmpname(). Keep them
-- inside the sandbox so every artifact is removed with the wrapper's temp dir.
local temp_seq = 0
os.tmpname = function()
	temp_seq = temp_seq + 1
	return STATE .. "/tmp." .. temp_seq
end

-- wait_running() uses os.time() to bound its polling loop. Use a virtual clock
-- so failed waits are fast and deterministic instead of sleeping for seconds.
local virtual_time = 1000000
os.time = function()
	virtual_time = virtual_time + 1
	return virtual_time
end

local CONFIG_JSON = [[{
	"exists": true,
	"multi": false,
	"revision": "rev1",
	"valid": true,
	"validation_error": "",
	"advanced_error": false,
	"unknown_fields": false,
	"server": "host:443",
	"server_identity": "host:443",
	"server_locked": false,
	"credential_generation": 1,
	"auth_set": true,
	"control_token_set": true,
	"tls_sni": "host",
	"tls_insecure": false,
	"client_name": "router",
	"mappings": [],
	"web_listen": "0.0.0.0:7501",
	"web_username": "admin",
	"password_set": true,
	"web_tls": false,
	"advanced": false,
	"log_level": "info",
	"log_file": ""
}]]

local FAKE_CLI = [=[
#!/bin/sh
set -u

state=__STATE__

cmd=${1:-}
case "$cmd" in
	config)
		sub=${2:-}
		case "$sub" in
			get)
				if [ -f "$state/get_fail" ]; then
					printf '%s\n' '{"ok":false,"error":"test configuration error","code":"internal"}' >&2
					exit 1
				fi
				if [ -f "$state/fail_get_on_start" ]; then
					if [ -f "$state/get_count" ]; then
						printf '%s\n' '{"ok":false,"error":"test readback failure","code":"internal"}' >&2
						exit 1
					fi
					: > "$state/get_count"
				fi
				cat "$state/config.json"
				;;
			set)
				if [ -f "$state/set_conflict" ]; then
					printf '%s\n' '{"ok":false,"error":"configuration changed on disk","code":"conflict"}' >&2
					exit 3
				fi
				if [ -f "$state/set_validation" ]; then
					printf '%s\n' '{"ok":false,"error":"invalid configuration","code":"validation"}' >&2
					exit 2
				fi
				cat > "$state/set-input.json"
				printf '%s\n' '{"ok":true,"revision":"new-revision"}'
				;;
			*)
				exit 1
				;;
		esac
		;;
	--check-config)
		if [ -f "$state/check_fail" ]; then
			printf '%s\n' 'test configuration error' >&2
			exit 1
		fi
		exit 0
		;;
	*)
		exit 1
		;;
esac
]=]

local FAKE_INIT = [=[
#!/bin/sh
set -u

state=__STATE__

case "${1:-}" in
	status)
		[ -f "$state/running" ]
		;;
	enabled)
		[ -f "$state/enabled" ]
		;;
	start)
		if [ -f "$state/start_fail" ]; then
			exit 1
		fi
		: > "$state/running"
		;;
	stop)
		rm -f "$state/running"
		;;
	restart)
		if [ -f "$state/start_fail" ]; then
			exit 1
		fi
		: > "$state/running"
		;;
	enable)
		if [ -f "$state/enable_fail" ]; then
			exit 1
		fi
		: > "$state/enabled"
		;;
	disable)
		if [ -f "$state/disable_fail" ]; then
			exit 1
		fi
		rm -f "$state/enabled"
		;;
	*)
		exit 1
		;;
esac
]=]

local function write_script(path, body)
	write_file(path, body:gsub("__STATE__", function()
		return shellquote(STATE)
	end))
	assert(sh("chmod 755 " .. shellquote(path)) == 0,
		"unable to make " .. path .. " executable")
end

local CLI = TEST_DIR .. "/hy2tun"
local INIT = TEST_DIR .. "/hy2tun.init"
write_script(CLI, FAKE_CLI)
write_script(INIT, FAKE_INIT)

-- luci.jsonc stub backed by lua-cjson.
package.loaded["luci.jsonc"] = {
	parse = function(text) return cjson.decode(text) end,
	stringify = function(value) return cjson.encode(value) end
}

-- luci.util stub.
package.loaded["luci.util"] = {
	shellquote = shellquote
}

-- nixio.fs stub. The controller must create private files with
-- nixio.open(path, "w", "600") instead of opening and chmodding afterwards.
-- Reject any path outside the sandbox so the test cannot read a host file.
local function sandbox_path(path)
	if type(path) ~= "string" or path:sub(1, #TEST_DIR + 1) ~= TEST_DIR .. "/" then
		error("nixio.fs escaped the test sandbox: " .. tostring(path), 2)
	end
end

local chmod_called = false
package.loaded["nixio.fs"] = {
	readfile = function(path)
		sandbox_path(path)
		return read_file(path)
	end,
	access = function(path)
		sandbox_path(path)
		return file_exists(path)
	end,
	chmod = function()
		chmod_called = true
		error("fs.chmod must not be used; use nixio.open(path, 'w', '600')", 2)
	end
}

-- nixio stub with a file-like object. Assert the controller requests the final
-- mode at creation time and only writes inside the sandbox.
local open_calls = {}
package.loaded["nixio"] = {
	open = function(path, mode, perm)
		open_calls[#open_calls + 1] = {
			path = path,
			mode = mode,
			perm = perm
		}

		if mode ~= "w" or perm ~= "600" then
			error("nixio.open must be called as nixio.open(path, 'w', '600')", 2)
		end
		if path:sub(1, #STATE + 1) ~= STATE .. "/" then
			error("nixio.open escaped the test sandbox: " .. tostring(path), 2)
		end

		local file, err = io.open(path, "w")
		if not file then
			return nil, err
		end

		return {
			write = function(_, data) return file:write(data) end,
			close = function(_)
				file:close()
				return true
			end
		}
	end
}

-- luci.http stub.
local http_stub = {
	form = {},
	body = nil,
	status_code = 200,
	content_type = nil
}

function http_stub.reset()
	http_stub.form = {}
	http_stub.body = nil
	http_stub.status_code = 200
	http_stub.content_type = nil
end

function http_stub.prepare_content(ctype)
	http_stub.content_type = ctype
end

function http_stub.write(data)
	http_stub.body = (http_stub.body or "") .. data
end

function http_stub.status(code)
	http_stub.status_code = code
end

function http_stub.formvalue(name)
	return http_stub.form[name]
end

package.loaded["luci.http"] = http_stub

-- luci.sys stub: run commands through the shell, but make sleep instant. The
-- guard makes an accidental production-path call fail the test immediately.
local commands = {}
package.loaded["luci.sys"] = {
	call = function(command)
		commands[#commands + 1] = command

		for _, forbidden in ipairs({
			"/etc/hy2tun",
			"/usr/bin/hy2tun",
			"/etc/init.d/hy2tun"
		}) do
			if command:find(forbidden, 1, true) then
				error("real production path used in command: " .. command, 2)
			end
		end

		if command:match("^sleep ") then
			return 0
		end

		return sh(command)
	end
}

-- luci.dispatcher stubs used by index().
local entries = {}
_G.entry = function(path, target, title, order)
	local node = {
		path = path,
		target = target,
		title = title,
		order = order
	}
	entries[#entries + 1] = node
	return node
end
_G.template = function(name) return { type = "template", name = name } end
_G.call = function(name) return { type = "call", ["function"] = name } end
_G.post = function(name) return { type = "call", ["function"] = name, post = true } end
_G._ = function(text) return text end

package.preload["luci.controller.hy2tun"] = function()
	assert(loadfile(CONTROLLER))()
	return package.loaded["luci.controller.hy2tun"]
end

local controller = require "luci.controller.hy2tun"

local function reset_state()
	assert(sh("rm -rf " .. shellquote(STATE)) == 0, "unable to clear test state")
	assert(sh("mkdir -p " .. shellquote(STATE)) == 0, "unable to create test state")
	write_file(STATE .. "/config.json", CONFIG_JSON)
end

local function read_config()
	return cjson.decode(read_file(STATE .. "/config.json"))
end

local function write_config(config)
	write_file(STATE .. "/config.json", cjson.encode(config))
end

local function decode_body()
	assert(http_stub.body, "no response body")
	return cjson.decode(http_stub.body)
end

local function call_data()
	http_stub.reset()
	controller.data()
	return decode_body(), http_stub.status_code, http_stub.content_type
end

local function call_action(form)
	http_stub.reset()
	http_stub.form = form
	controller.action()
	return decode_body(), http_stub.status_code, http_stub.content_type
end

local function check(condition, message)
	if not condition then
		error("FAIL: " .. message, 2)
	end
end

local function touch(name)
	write_file(STATE .. "/" .. name, "")
end

local function remove(name)
	remove_file(STATE .. "/" .. name)
end

local function run()
	reset_state()

	-- Route registration.
	entries = {}
	controller.index()
	local by_path = {}
	for _, node in ipairs(entries) do
		by_path[table.concat(node.path, "/")] = node
	end
	check(by_path["admin/services/hy2tun"], "page route missing")
	check(by_path["admin/services/hy2tun/data"], "data route missing")
	check(by_path["admin/services/hy2tun/action"], "action route missing")
	check(by_path["admin/services/hy2tun/data"].leaf == true, "data must be leaf")
	check(by_path["admin/services/hy2tun/action"].leaf == true, "action must be leaf")
	check(by_path["admin/services/hy2tun"].target.name == "hy2tun/status",
		"page route must render the status template")

	-- LuCI reads `post` from the call()/post() target, not the route node.
	local data_node = by_path["admin/services/hy2tun/data"]
	check(type(data_node.target) == "table", "data target missing")
	check(data_node.target["function"] == "data", "data target function")
	check(data_node.target.post ~= true, "data target must not be POST")

	local action_node = by_path["admin/services/hy2tun/action"]
	check(type(action_node.target) == "table", "action target missing")
	check(action_node.target["function"] == "action", "action target function")
	check(action_node.target.post == true, "action target must be POST")
	check(action_node.post ~= true, "post must not live only on the route node")

	-- The page must stay hidden when the init script is absent.
	local fs_stub = package.loaded["nixio.fs"]
	local old_access = fs_stub.access
	fs_stub.access = function() return false end
	entries = {}
	controller.index()
	check(#entries == 0, "index must not register routes without the init script")
	fs_stub.access = old_access

	-- data success.
	local body, status, ctype = call_data()
	check(status == 200, "data status")
	check(ctype == "application/json", "data content type")
	check(body.ok == true, "data ok")
	check(body.running == false, "data running")
	check(body.enabled == false, "data enabled")
	check(body.config and body.config.revision == "rev1", "data config")
	check(body.config_error == "", "data config_error")

	-- data with config failure still returns status and logs.
	touch("get_fail")
	body = call_data()
	check(body.ok == true, "data failure ok")
	check(body.config == cjson.null, "data failure config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"data failure config_error")
	check(body.running == false and body.enabled == false, "data failure status")
	check(type(body.logs) == "string", "data failure logs")
	remove("get_fail")

	-- save/save_start are rejected while running. Page dirty-state gating is
	-- JavaScript-only (the page harness covers it); the controller-side
	-- equivalent is this stop-before-save rule and keeping saved=true when a
	-- save_start's start step fails.
	touch("running")
	body, status = call_action({ op = "save", payload = '{"server":"host:443"}' })
	check(status == 400, "save running status")
	check(body.ok == false, "save running ok")
	check(body.error:find("stop"), "save running error")
	body, status = call_action({ op = "save_start", payload = '{"server":"host:443"}' })
	check(status == 400, "save_start running status")
	check(body.ok == false, "save_start running ok")
	check(body.error:find("stop"), "save_start running error")
	remove("running")

	-- save conflict.
	touch("set_conflict")
	body, status = call_action({ op = "save", payload = '{"server":"host:443"}' })
	check(status == 409, "save conflict status")
	check(body.ok == false, "save conflict ok")
	check(body.error:find("changed"), "save conflict error")
	remove("set_conflict")

	-- save validation.
	touch("set_validation")
	body, status = call_action({ op = "save", payload = '{"server":"host:443"}' })
	check(status == 400, "save validation status")
	check(body.ok == false, "save validation ok")
	remove("set_validation")

	-- save success. The controller must create the payload file with the final
	-- mode, not open it and call fs.chmod() afterwards.
	open_calls = {}
	body, status = call_action({ op = "save", payload = '{"server":"host:443"}' })
	check(status == 200, "save success status")
	check(body.ok == true, "save success ok")
	check(body.saved == true, "save success saved")
	check(body.revision == "new-revision", "save success revision")
	check(body.config and body.config.revision == "rev1", "save success config")
	check(read_file(STATE .. "/set-input.json") == '{"server":"host:443"}',
		"payload passed unchanged")
	check(#open_calls > 0, "save must create the payload file with nixio.open")
	for _, call in ipairs(open_calls) do
		check(call.mode == "w", "nixio.open mode must be w")
		check(call.perm == "600", "nixio.open mode must be 600")
	end
	check(chmod_called == false, "controller must not call fs.chmod")

	-- save succeeds but the read-back fails: report saved=true and config_error.
	touch("get_fail")
	body, status = call_action({ op = "save", payload = '{"server":"host:443"}' })
	check(status == 500, "save readback status")
	check(body.ok == false, "save readback ok")
	check(body.saved == true, "save readback saved")
	check(body.config == cjson.null, "save readback config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"save readback config_error")
	remove("get_fail")

	-- save_start with check-config failure: saved but not started.
	touch("check_fail")
	body, status = call_action({ op = "save_start", payload = '{"server":"host:443"}' })
	check(status == 400, "save_start check status")
	check(body.ok == false, "save_start check ok")
	check(body.saved == true, "save_start check saved")
	check(body.revision == "new-revision", "save_start check revision")
	check(body.config and body.config.revision == "rev1", "save_start check config")
	check(body.error:find("test configuration error"), "save_start check error")
	remove("check_fail")

	-- save_start succeeds at check-config but the service fails to start:
	-- the config must stay saved and the response must include the new
	-- revision and logs.
	touch("start_fail")
	body, status = call_action({ op = "save_start", payload = '{"server":"host:443"}' })
	check(status == 500, "save_start start failure status")
	check(body.ok == false, "save_start start failure ok")
	check(body.saved == true, "save_start start failure saved")
	check(body.revision == "new-revision", "save_start start failure revision")
	check(body.config and body.config.revision == "rev1",
		"save_start start failure config")
	check(type(body.error) == "string" and body.error ~= "",
		"save_start start failure error")
	check(type(body.logs) == "string", "save_start start failure logs")
	remove("start_fail")

	-- save_start success.
	body, status = call_action({ op = "save_start", payload = '{"server":"host:443"}' })
	check(status == 200, "save_start success status")
	check(body.ok == true, "save_start success ok")
	check(body.saved == true, "save_start success saved")
	check(body.running == true, "save_start success running")
	remove("running")

	-- start is blocked when the single-server Web console has no password.
	local config = read_config()
	config.password_set = false
	write_config(config)
	body, status = call_action({ op = "start" })
	check(status == 400, "start web password status")
	check(body.ok == false, "start web password ok")
	check(body.error:find("password"), "start web password error")
	config.password_set = true
	write_config(config)

	-- Multi-server configs skip the Web-password gate.
	config.multi = true
	config.password_set = false
	write_config(config)
	body, status = call_action({ op = "start" })
	check(status == 200, "multi start status")
	check(body.ok == true and body.running == true, "multi start ok")
	remove("running")
	config.multi = false
	config.password_set = true
	write_config(config)

	-- restart rejected while stopped.
	body, status = call_action({ op = "restart" })
	check(status == 400, "restart stopped status")
	check(body.ok == false, "restart stopped ok")
	check(body.error:find("use start"), "restart stopped error")

	-- start with check-config failure.
	touch("check_fail")
	body, status = call_action({ op = "start" })
	check(status == 400, "start check status")
	check(body.ok == false, "start check ok")
	remove("check_fail")

	-- start success.
	body, status = call_action({ op = "start" })
	check(status == 200, "start success status")
	check(body.ok == true, "start success ok")
	check(body.running == true, "start success running")

	-- A post-action config read failure must still surface config_error even
	-- when the pre-action config is used as a fallback.
	touch("fail_get_on_start")
	body, status = call_action({ op = "start" })
	check(status == 200, "start readback status")
	check(body.ok == true, "start readback ok")
	check(body.running == true, "start readback running")
	check(body.config and body.config.revision == "rev1",
		"start readback fallback config")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"start readback config_error")
	remove("fail_get_on_start")
	remove("get_count")

	-- restart success.
	body, status = call_action({ op = "restart" })
	check(status == 200, "restart success status")
	check(body.ok == true, "restart success ok")
	check(body.running == true, "restart success running")

	-- stop success.
	body, status = call_action({ op = "stop" })
	check(status == 200, "stop success status")
	check(body.ok == true, "stop success ok")
	check(body.running == false, "stop success running")

	-- enable / disable.
	body, status = call_action({ op = "enable" })
	check(status == 200 and body.ok == true and body.enabled == true, "enable")
	body, status = call_action({ op = "disable" })
	check(status == 200 and body.ok == true and body.enabled == false, "disable")

	-- config_error propagation on enable/disable.
	touch("get_fail")
	body, status = call_action({ op = "enable" })
	check(status == 200, "enable config error status")
	check(body.ok == true and body.enabled == true, "enable config error state")
	check(body.config == cjson.null, "enable config error config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"enable config error")
	body, status = call_action({ op = "disable" })
	check(status == 200, "disable config error status")
	check(body.ok == true and body.enabled == false, "disable config error state")
	check(body.config == cjson.null, "disable config error config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"disable config error")
	remove("get_fail")

	-- config_error propagation on logs.
	touch("get_fail")
	body, status = call_action({ op = "logs" })
	check(status == 200, "logs config error status")
	check(body.ok == true, "logs config error ok")
	check(body.config == cjson.null, "logs config error config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"logs config error")
	check(type(body.logs) == "string", "logs config error logs")
	remove("get_fail")

	-- config_error propagation on start.
	touch("get_fail")
	body, status = call_action({ op = "start" })
	check(status == 500, "start config error status")
	check(body.ok == false, "start config error ok")
	check(body.config == cjson.null, "start config error config null")
	check(type(body.config_error) == "string" and body.config_error ~= "",
		"start config error")
	check(body.error:find("cannot read configuration"),
		"start config error message")
	remove("get_fail")

	-- logs from log_file.
	local log_path = STATE .. "/hy2tun.log"
	write_file(log_path, "line one\nline two\n")
	config = read_config()
	config.log_file = log_path
	write_config(config)
	body, status = call_action({ op = "logs" })
	check(status == 200 and body.ok == true, "logs status")
	check(body.logs:find("line one") and body.logs:find("line two"),
		"logs from log_file")

	-- data also uses log_file.
	body, status = call_data()
	check(status == 200 and body.ok == true, "data logs status")
	check(body.logs:find("line one") and body.logs:find("line two"),
		"data logs from log_file")
	config.log_file = ""
	write_config(config)

	-- unknown op.
	body, status = call_action({ op = "bogus" })
	check(status == 400, "unknown op status")
	check(body.ok == false, "unknown op ok")

	-- malformed payload.
	body, status = call_action({ op = "save", payload = "not-json" })
	check(status == 400, "malformed payload status")
	check(body.ok == false, "malformed payload ok")

	-- payload must be an object, not an array.
	body, status = call_action({ op = "save", payload = "[1,2]" })
	check(status == 400, "array payload status")
	check(body.ok == false, "array payload ok")

	check(#commands > 0, "controller did not run any test commands")
	print("controller harness: all checks passed")
end

local ok, err = pcall(run)
sh("rm -rf " .. shellquote(STATE))
if not ok then
	io.stderr:write(tostring(err) .. "\n")
	os.exit(1)
end
