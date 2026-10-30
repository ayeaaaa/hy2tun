module("luci.controller.hy2tun", package.seeall)

local fs = require "nixio.fs"
local nixio = require "nixio"
local jsonc = require "luci.jsonc"
local http = require "luci.http"
local sys = require "luci.sys"
local util = require "luci.util"

local CONFIG_PATH = "/etc/hy2tun/client.yaml"
local BINARY = "/usr/bin/hy2tun"
local INIT = "/etc/init.d/hy2tun"

local JSON_NULL = {}

local STATUS_TEXT = {
	[200] = "OK",
	[400] = "Bad Request",
	[403] = "Forbidden",
	[404] = "Not Found",
	[405] = "Method Not Allowed",
	[409] = "Conflict",
	[500] = "Internal Server Error"
}

local ALLOWED_OPS = {
	save = true,
	save_start = true,
	start = true,
	stop = true,
	restart = true,
	enable = true,
	disable = true,
	logs = true
}

local SERVICE_OPS = {
	start = true,
	stop = true,
	restart = true,
	enable = true,
	disable = true
}

local function shellquote(value)
	return util.shellquote(value)
end

local function temp_file()
	local ok, path = pcall(os.tmpname)
	if ok and type(path) == "string" and path ~= "" then
		return path
	end
	return nil
end

local function read_file(path)
	return fs.readfile(path) or ""
end

local function write_file(path, data)
	-- Create with the final mode instead of opening first and chmodding
	-- afterwards. nixio expects the symbolic string mode.
	local file = nixio.open(path, "w", "600")
	if not file then
		return false
	end

	local written = file:write(data)
	file:close()

	return written and true or false
end

local function remove_file(path)
	if path then
		os.remove(path)
	end
end

local function parse_json(text)
	if type(text) ~= "string" or text == "" then
		return nil
	end

	local ok, value = pcall(jsonc.parse, text)
	if ok and type(value) == "table" then
		return value
	end

	return nil
end

local function first_line(text)
	if type(text) ~= "string" then
		return ""
	end

	return text:match("^%s*([^\r\n]*)") or ""
end

-- Run a fixed command with both stdout and stderr redirected to private
-- temporary files. The init script and CLI must never write into the CGI
-- response directly.
local function run_capture(command)
	local out_path = temp_file()
	local err_path = temp_file()

	if not out_path or not err_path then
		remove_file(out_path)
		remove_file(err_path)
		return -1, "", "unable to create a temporary file"
	end

	-- Group the command so pipeline stderr is redirected as a whole. A bare
	-- trailing redirection would only apply to the last pipeline element.
	-- Redirect stderr first so shell redirection errors are captured too.
	local ok, code = pcall(sys.call, "( " .. command .. " )" ..
		" 2> " .. shellquote(err_path) ..
		" > " .. shellquote(out_path))

	if not ok then
		remove_file(out_path)
		remove_file(err_path)
		return -1, "", "failed to run command"
	end

	local out = read_file(out_path)
	local err = read_file(err_path)

	remove_file(out_path)
	remove_file(err_path)

	return code, out, err
end

-- Run a fixed init command with a portable shell watchdog. OpenWrt's BusyBox
-- does not necessarily provide `timeout`, so use background jobs and wait.
local function run_bounded(seconds, command)
	return sys.call(
		"(" .. command .. ") >/dev/null 2>&1 & child=$!; " ..
		"(sleep " .. seconds .. "; kill -9 $child 2>/dev/null) & watchdog=$!; " ..
		"wait $child; rc=$?; kill $watchdog 2>/dev/null; exit $rc"
	)
end

local function service_running()
	return run_bounded(2, INIT .. " status") == 0
end

local function service_enabled()
	return run_bounded(2, INIT .. " enabled") == 0
end

local function run_service(op)
	if not SERVICE_OPS[op] then
		return 1
	end

	return run_bounded(10, INIT .. " " .. op)
end

local function wait_running(want, timeout)
	timeout = timeout or 5
	local deadline = os.time() + timeout

	while os.time() < deadline do
		if service_running() == want then
			return true
		end
		local remaining = deadline - os.time()
		if remaining <= 0 then
			break
		end
		sys.call("sleep " .. remaining .. " >/dev/null 2>&1")
	end

	return false
end

local function code_for_exit(code)
	if code == 2 then
		return "validation"
	elseif code == 3 then
		return "conflict"
	elseif code == 4 then
		return "unknown_fields"
	elseif code == 5 then
		return "advanced"
	elseif code == 6 then
		return "service_running"
	end

	return "internal"
end

local function status_for_code(code)
	if code == "conflict" then
		return 409
	elseif code == "service_running" or code == "validation" or
		code == "advanced" or code == "unknown_fields" or
		code == "multi" or code == "parse_error" then
		return 400
	end

	return 500
end

-- Returns config, error message, error code. The CLI always writes a stable
-- JSON error to stderr on failure, so only its error text is exposed.
local function config_get()
	local code, out, err = run_capture(
		BINARY .. " config get -c " .. shellquote(CONFIG_PATH))

	if code == 0 then
		local config = parse_json(out)
		if not config then
			return nil, "invalid JSON from hy2tun config get", "internal"
		end
		return config, nil, nil
	end

	local info = parse_json(err)
	if info then
		local message = type(info.error) == "string" and info.error ~= "" and
			info.error or "hy2tun config get failed"
		local error_code = type(info.code) == "string" and info.code ~= "" and
			info.code or "internal"
		return nil, message, error_code
	end

	return nil, "hy2tun config get failed", "internal"
end

-- Returns saved, result, error info. The payload is written to a private
-- temporary file and passed on stdin; it is never placed on the command line.
local function config_set(payload)
	local in_path = temp_file()
	local out_path = temp_file()
	local err_path = temp_file()

	if not in_path or not out_path or not err_path then
		remove_file(in_path)
		remove_file(out_path)
		remove_file(err_path)
		return false, nil, {
			code = "internal",
			error = "unable to create a temporary file",
			advanced = false
		}
	end

	if not write_file(in_path, payload) then
		remove_file(in_path)
		remove_file(out_path)
		remove_file(err_path)
		return false, nil, {
			code = "internal",
			error = "unable to write the request payload",
			advanced = false
		}
	end

	local ok, code = pcall(sys.call,
		BINARY .. " config set -c " .. shellquote(CONFIG_PATH) ..
		" --require-stopped" ..
		" 2> " .. shellquote(err_path) ..
		" < " .. shellquote(in_path) ..
		" > " .. shellquote(out_path))

	if not ok then
		remove_file(in_path)
		remove_file(out_path)
		remove_file(err_path)
		return false, nil, {
			code = "internal",
			error = "failed to run hy2tun config set",
			advanced = false
		}
	end

	local out = read_file(out_path)
	local err = read_file(err_path)

	remove_file(in_path)
	remove_file(out_path)
	remove_file(err_path)

	local result = parse_json(out)
	if code == 0 then
		return true, result, nil
	end

	local info = parse_json(err)
	if not info then
		info = {
			ok = false,
			code = code_for_exit(code),
			error = "hy2tun config set failed",
			advanced = false
		}
	end

	if type(info.code) ~= "string" or info.code == "" then
		info.code = code_for_exit(code)
	end

	if type(info.error) ~= "string" or info.error == "" then
		info.error = "hy2tun config set failed"
	end

	info.advanced = info.advanced == true

	return false, nil, info
end

local function check_config()
	local code, out, err = run_capture(
		BINARY .. " --check-config -c " .. shellquote(CONFIG_PATH))

	if code == 0 then
		return true, nil
	end

	local message = first_line(err)
	if message == "" then
		message = first_line(out)
	end
	if message == "" then
		message = "configuration check failed"
	end

	return false, message
end

local function start_precondition(config)
	if type(config) ~= "table" then
		return false, "configuration is not available", 400
	end

	-- Multi-server configurations let the client generate its Web password;
	-- the single-server form requires a password before starting.
	if config.multi ~= true then
		local listen = config.web_listen
		if type(listen) == "string" and listen ~= "" and
			config.password_set ~= true then
			return false,
				"web console is enabled but no password is set; set a password before starting",
				400
		end
	end

	local ok, err = check_config()
	if not ok then
		return false, err, 400
	end

	return true, nil, 200
end

local function start_service(op, config)
	local ok, err, status = start_precondition(config)
	if not ok then
		return false, err, status
	end

	local code = run_service(op)
	if not wait_running(true, 5) then
		if code ~= 0 then
			return false, "failed to " .. op .. " the hy2tun service", 500
		end

		return false, "hy2tun did not become running within 5 seconds", 500
	end

	return true, nil, 200
end

local function read_logs(config)
	local log_file = type(config) == "table" and config.log_file or nil
	local command

	if type(log_file) == "string" and log_file ~= "" then
		command = "tail -n 50 -- " .. shellquote(log_file)
	else
		command = "logread | grep hy2tun | tail -50"
	end

	local _, out = run_capture(command)
	return out or ""
end

local function encode_json_value(value)
	if value == nil or value == JSON_NULL then
		return "null"
	end

	local encoded = jsonc.stringify(value)
	if type(encoded) ~= "string" then
		return "null"
	end

	return encoded
end

-- luci.jsonc.stringify() omits nil table values, so encode the envelope
-- ourselves to keep an explicit "config": null when config_get() fails.
local function write_json(object)
	local keys = {}
	for key in pairs(object) do
		keys[#keys + 1] = key
	end
	table.sort(keys)

	local parts = {}
	for _, key in ipairs(keys) do
		parts[#parts + 1] = jsonc.stringify(key) .. ":" ..
			encode_json_value(object[key])
	end

	http.prepare_content("application/json")
	http.write("{" .. table.concat(parts, ",") .. "}")
end

local function send_json(status, object)
	if status and status ~= 200 then
		http.status(status, STATUS_TEXT[status] or "Error")
	end

	write_json(object)
end

local function response()
	return {
		ok = true,
		config = JSON_NULL,
		running = false,
		enabled = false,
		logs = "",
		advanced = false,
		error = "",
		config_error = "",
		saved = false,
		revision = ""
	}
end

local function send_action(status, res, config, config_error)
	if type(config) == "table" then
		res.config = config
		if res.revision == "" and type(config.revision) == "string" then
			res.revision = config.revision
		end
		if config.advanced == true then
			res.advanced = true
		end
	else
		res.config = JSON_NULL
		if res.config_error == "" then
			res.config_error = "configuration is unavailable"
		end
	end

	if type(config_error) == "string" and config_error ~= "" then
		res.config_error = config_error
	end

	res.running = service_running()
	res.enabled = service_enabled()
	res.logs = read_logs(config)

	send_json(status, res)
end

local function action_error(status, message, info)
	local res = response()
	res.ok = false
	res.error = message or "request failed"

	if type(info) == "table" and info.advanced == true then
		res.advanced = true
	end

	local config, config_error = config_get()
	send_action(status, res, config, config_error)
end

function data()
	local config, config_error = config_get()

	write_json({
		ok = true,
		running = service_running(),
		enabled = service_enabled(),
		config = config or JSON_NULL,
		config_error = config_error or "",
		logs = read_logs(config)
	})
end

function action()
	local op = http.formvalue("op")

	if type(op) ~= "string" or not ALLOWED_OPS[op] then
		return action_error(400, "unknown action")
	end

	if op == "save" or op == "save_start" then
		local payload = http.formvalue("payload")

		if type(payload) ~= "string" or payload == "" then
			return action_error(400, "payload is required")
		end

		local parsed_ok, update = pcall(jsonc.parse, payload)
		if not parsed_ok or type(update) ~= "table" or update[1] ~= nil then
			return action_error(400, "payload must be a JSON object")
		end

		if service_running() then
			return action_error(400,
				"stop the hy2tun service before changing the configuration")
		end

		local saved, set_result, set_error = config_set(payload)
		if not saved then
			local code = "internal"
			local message = "failed to save the configuration"
			if type(set_error) == "table" then
				if type(set_error.code) == "string" and set_error.code ~= "" then
					code = set_error.code
				end
				if type(set_error.error) == "string" and set_error.error ~= "" then
					message = set_error.error
				end
			end
			return action_error(status_for_code(code), message, set_error)
		end

		local config, config_error = config_get()
		local revision = type(set_result) == "table" and set_result.revision or nil
		if type(revision) ~= "string" or revision == "" then
			revision = type(config) == "table" and config.revision or ""
		end

		local res = response()
		res.saved = true
		res.revision = revision or ""

		if not config then
			res.ok = false
			res.error = "configuration was saved but could not be read back: " ..
				(config_error or "unknown error")
			return send_action(500, res, nil, config_error)
		end

		if op == "save" then
			res.ok = true
			return send_action(200, res, config, nil)
		end

		-- save_start: the configuration is already committed. A start
		-- failure must report saved=true so the page keeps the new revision.
		local started, start_error, start_status = start_service("start", config)
		if not started then
			res.ok = false
			res.error = start_error or "failed to start the hy2tun service"
			return send_action(start_status or 500, res, config, nil)
		end

		res.ok = true
		return send_action(200, res, config, nil)
	end

	if op == "start" or op == "restart" then
		if op == "restart" and not service_running() then
			return action_error(400, "hy2tun is not running; use start")
		end

		local config, config_error, config_code = config_get()
		if not config then
			return action_error(status_for_code(config_code),
				"cannot read configuration: " .. (config_error or "unknown error"))
		end

		local started, start_error, start_status = start_service(op, config)
		local new_config, new_error = config_get()
		if not new_config then
			new_config = config
		end

		local res = response()
		res.ok = started
		if not started then
			res.error = start_error or ("failed to " .. op .. " the hy2tun service")
		end

		return send_action(started and 200 or (start_status or 500),
			res, new_config, new_error)
	end

	if op == "stop" then
		run_service("stop")
		local stopped = wait_running(false, 5)

		local config, config_error = config_get()
		local res = response()
		res.ok = stopped
		if not stopped then
			res.error = "hy2tun did not stop within 5 seconds"
		end

		return send_action(stopped and 200 or 500, res, config, config_error)
	end

	if op == "enable" or op == "disable" then
		local code = run_service(op)
		local config, config_error = config_get()

		local res = response()
		if code ~= 0 then
			res.ok = false
			res.error = "failed to " .. op .. " the hy2tun service"
			return send_action(500, res, config, config_error)
		end

		res.ok = true
		return send_action(200, res, config, config_error)
	end

	-- logs
	local config, config_error = config_get()
	local res = response()
	res.ok = true
	return send_action(200, res, config, config_error)
end

function index()
	if not fs.access("/etc/init.d/hy2tun") then
		return
	end

	entry({"admin", "services", "hy2tun"}, template("hy2tun/status"), _("hy2tun"), 60).dependent = false

	local data = entry({"admin", "services", "hy2tun", "data"}, call("data"), nil)
	data.leaf = true

	local action = entry({"admin", "services", "hy2tun", "action"}, post("action"), nil)
	action.leaf = true
end
