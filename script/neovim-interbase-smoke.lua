-- Read-only SQLs/Neovim acceptance against a disposable InterBase fixture.
-- Requires SQLS_NATIVE_TARGET, SQLS_NATIVE_USER, SQLS_NATIVE_PASSWORD and
-- SQLS_NATIVE_BINARY. The fixture itself is created/removed by the caller.
local function check(ok, message)
  if not ok then error("SQLS native Neovim acceptance: " .. message, 2) end
end

local function run()
  local target = assert(vim.env.SQLS_NATIVE_TARGET, "SQLS_NATIVE_TARGET required")
  local user = assert(vim.env.SQLS_NATIVE_USER, "SQLS_NATIVE_USER required")
  local password = assert(vim.env.SQLS_NATIVE_PASSWORD, "SQLS_NATIVE_PASSWORD required")
  local binary = assert(vim.env.SQLS_NATIVE_BINARY, "SQLS_NATIVE_BINARY required")
  check(vim.fn.executable(binary) == 1, "binary is not executable")
  local failures = {}
  local id = vim.lsp.start({
    name = "sqls-disposable-interbase-acceptance",
    cmd = { binary }, root_dir = vim.fn.getcwd(),
    init_options = { connectionConfig = {
      alias = "disposable", driver = "interbase", dataSourceName = target,
      user = user, passwd = password, dialect = 3, params = { charset = "UTF8" },
    } },
    on_error = function(code, err) failures[#failures + 1] = tostring(code) .. ": " .. tostring(err) end,
    on_exit = function(code) if code ~= 0 then failures[#failures + 1] = "server exit " .. tostring(code) end end,
  })
  check(id ~= nil, "could not start LSP client")
  local client
  check(vim.wait(15000, function()
    client = vim.lsp.get_client_by_id(id)
    return client ~= nil and client.initialized
  end, 25), "initialize timed out")
  local buf = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(buf, vim.fn.tempname() .. ".sql")
  vim.bo[buf].filetype = "sql"
  vim.api.nvim_buf_set_lines(buf, 0, -1, false, { "SELECT * FROM MDE_SMOKE" })
  vim.api.nvim_set_current_buf(buf)
  check(vim.lsp.buf_attach_client(buf, id), "could not attach SQL document")
  local uri = vim.uri_from_bufnr(buf)
  local function request(method, params)
    local result = client:request_sync(method, params, 12000, buf)
    check(result ~= nil, method .. " timed out")
    check(result.err == nil, method .. " failed: " .. vim.inspect(result.err))
    return result.result
  end
  local function status()
    return request("workspace/executeCommand", { command = "sqls.showMetadataStatus" })
  end
  check(vim.wait(45000, function()
    local ok, value = pcall(status)
    if not ok then return false end
    return value.connectionState == "ready" and value.settled == true
      and value.degraded ~= true and (value.generation or 0) > 0
  end, 50), "native metadata not ready")
  local params = { textDocument = { uri = uri }, position = { line = 0, character = 18 } }
  local hover = request("textDocument/hover", params)
  check(hover and vim.inspect(hover.contents):find("MDE_SMOKE", 1, true), "table hover missing")
  local definitions = request("textDocument/definition", params)
  check(type(definitions) == "table" and #definitions >= 1, "table definition missing")
  local definition = definitions[1]
  check(definition.uri ~= uri and definition.uri:sub(1, 5) == "file:", "source URI was not materialized")
  local f = assert(io.open(vim.uri_to_fname(definition.uri), "rb"))
  local body = f:read("*a"); f:close()
  check(body:find('CREATE TABLE "MDE_SMOKE"', 1, true) ~= nil,
    "native table DDL missing; snapshot body=" .. vim.inspect(body:sub(1, 650)))
  local query = request("workspace/executeCommand", { command = "executeQuery", arguments = { uri } })
  check(type(query) == "string" and query:find("B4_NATIVE_MARKER", 1, true) ~= nil
      and query:find("SECOND_MARKER", 1, true) ~= nil
      and query:find("2 rows in set", 1, true) ~= nil,
    "native query did not return fixture rows: " .. vim.inspect(query))
  local plan = request("workspace/executeCommand", { command = "explainQuery", arguments = { uri } })
  check(type(plan) == "string" and plan:find("PLAN", 1, true) ~= nil,
    "native explain did not return a plan: " .. vim.inspect(plan))
  client:stop(5000)
  check(vim.wait(10000, function() return vim.lsp.get_client_by_id(id) == nil end, 25), "server did not stop")
  check(#failures == 0, "unexpected client errors: " .. vim.inspect(failures))
  print("SQLS disposable InterBase Neovim acceptance: PASS")
end

local ok, err = pcall(run)
if not ok then
  io.stderr:write(tostring(err) .. "\n")
  vim.cmd("cq")
end
