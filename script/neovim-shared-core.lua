-- End-to-end acceptance client for SQLS's deterministic test-only stdio LSP
-- fixture. Run from the SQLS repository with SQLS_NEOVIM_FIXTURE=1.

local function fail(message)
  error("SQLS Neovim acceptance: " .. message, 2)
end

local function assert_equal(actual, expected, label)
  if not vim.deep_equal(actual, expected) then
    fail((label or "value") .. " mismatch\nexpected: " .. vim.inspect(expected) .. "\nactual: " .. vim.inspect(actual))
  end
end

local function contains(text, needle)
  return type(text) == "string" and text:find(needle, 1, true) ~= nil
end

local function wait_for(label, predicate, timeout_ms)
  local last_error
  local ok = vim.wait(timeout_ms or 8000, function()
    local status, value = pcall(predicate)
    if not status then
      last_error = value
      return false
    end
    return value == true
  end, 20)
  if not ok then
    fail("timed out waiting for " .. label .. (last_error and (": " .. tostring(last_error)) or ""))
  end
end

local function request(client, method, params, bufnr)
  local response = client:request_sync(method, params, 8000, bufnr)
  if not response then
    fail(method .. " timed out")
  end
  if response.err then
    fail(method .. " returned LSP error: " .. vim.inspect(response.err))
  end
  return response.result
end

local function uri_for(bufnr)
  return vim.uri_from_bufnr(bufnr)
end

local function open_buffer(client_id, filename, text)
  local bufnr = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(bufnr, filename)
  vim.api.nvim_buf_set_option(bufnr, "filetype", "sql")
  vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, vim.split(text, "\n", { plain = true }))
  vim.api.nvim_set_current_buf(bufnr)
  local attached = vim.lsp.buf_attach_client(bufnr, client_id)
  if not attached then
    fail("Neovim did not attach SQL buffer " .. filename)
  end
  return bufnr
end

local function position(line, character)
  return { line = line, character = character }
end

local function text_document(bufnr)
  return { uri = uri_for(bufnr) }
end

local function text_document_position(bufnr, line, character)
  return { textDocument = text_document(bufnr), position = position(line, character) }
end

local function read_file(path)
  local file, err = io.open(path, "rb")
  if not file then
    fail("cannot read definition snapshot " .. path .. ": " .. tostring(err))
  end
  local text = file:read("*a")
  file:close()
  return text
end

local function assert_formatting_edits(edits, original)
  local formatted = edits and edits[1] and edits[1].newText or ""
  local want_range = { start = position(0, 0), ["end"] = position(1, 0) }
  if type(edits) ~= "table" or #edits ~= 1 or formatted ~= "SELECT\n  *\nFROM\n  customers"
      or not vim.deep_equal(edits[1].range, want_range) then
    fail("formatting edits mismatch: " .. vim.inspect(edits) .. " for " .. vim.inspect(original))
  end
end

local function assert_reselected_metadata(before, after)
  if after.connectionState ~= "ready" or after.settled ~= true or after.degraded == true then
    fail("reselected metadata is not ready: " .. vim.inspect(after))
  end
  if type(after.connectionGeneration) ~= "number" or after.connectionGeneration <= before.connectionGeneration then
    fail("same-target reselect did not advance connection generation: before=" .. vim.inspect(before)
      .. " after=" .. vim.inspect(after))
  end
  if type(after.generation) ~= "number" or after.generation <= before.generation then
    fail("explicit metadata Refresh did not advance lease generation: before=" .. vim.inspect(before)
      .. " after=" .. vim.inspect(after))
  end
  -- Lease.Refresh resets Revision within the new generation, so Revision is
  -- captured for evidence/coherence but is not a globally monotonic counter.
  if type(after.revision) ~= "number" or after.revision < 1 then
    fail("refreshed generation has no published metadata revision: before revision=" .. tostring(before.revision)
      .. " after=" .. vim.inspect(after))
  end
end

local function run()
  if vim.fn.executable(".local/sqls-handler-tests") ~= 1 then
    fail("missing .local/sqls-handler-tests; compile internal/handler with go test -c first")
  end

  local unexpected = {}
  local fixture_entries = {}
  local diagnostics_by_uri = {}
  local original_diagnostics_handler = vim.lsp.handlers["textDocument/publishDiagnostics"]
  local diagnostics_handler = function(err, result, ctx, config)
    local ok, detail = pcall(function()
      if err then
        unexpected[#unexpected + 1] = "publishDiagnostics: " .. vim.inspect(err)
      elseif result then
        diagnostics_by_uri[result.uri] = diagnostics_by_uri[result.uri] or {}
        diagnostics_by_uri[result.uri][result.version] = vim.deepcopy(result.diagnostics or {})
      end
      if original_diagnostics_handler then
        original_diagnostics_handler(err, result, ctx, config)
      end
    end)
    if not ok then
      unexpected[#unexpected + 1] = "publishDiagnostics handler threw: " .. tostring(detail)
    end
  end

  local name = "sqls-neovim-shared-core-acceptance"
  local config = {
    name = name,
    cmd = { ".local/sqls-handler-tests", "-test.run=^TestNeovimFixtureServer$" },
    cmd_env = { SQLS_NEOVIM_FIXTURE = "1" },
    root_dir = vim.fn.getcwd(),
    init_options = {
      connectionConfig = {
        alias = "old",
        driver = "sqls-neovim-fixture",
        dataSourceName = "fixture://old",
      },
    },
    handlers = {
      ["textDocument/publishDiagnostics"] = diagnostics_handler,
      ["sqls/fixtureQueryEntered"] = function(err, result)
        if err then
          unexpected[#unexpected + 1] = "fixtureQueryEntered: " .. vim.inspect(err)
        else
          fixture_entries[#fixture_entries + 1] = vim.deepcopy(result or {})
        end
      end,
    },
    before_init = function(_, _)
      -- Neovim's initialized notification is sent immediately after initialize
      -- responds. Queue the profiles while the server has not yet initialized.
      local clients = vim.lsp.get_clients({ name = name, _uninitialized = true })
      if #clients ~= 1 then
        fail("before_init expected exactly one registered LSP client; got " .. #clients)
      end
      clients[1]:notify("workspace/didChangeConfiguration", {
        settings = {
          sqls = {
            connections = {
              { alias = "old", driver = "sqls-neovim-fixture", dataSourceName = "fixture://old" },
              { alias = "new", driver = "sqls-neovim-fixture", dataSourceName = "fixture://new" },
            },
          },
        },
      })
    end,
    on_error = function(code, err)
      unexpected[#unexpected + 1] = "client error " .. tostring(code) .. ": " .. tostring(err)
    end,
    on_exit = function(code, signal)
      if code ~= 0 then
        unexpected[#unexpected + 1] = "fixture exited with code=" .. tostring(code) .. " signal=" .. tostring(signal)
      end
    end,
  }

  local first_buf = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(first_buf, vim.fn.tempname() .. ".sql")
  vim.api.nvim_buf_set_option(first_buf, "filetype", "sql")
  vim.api.nvim_buf_set_lines(first_buf, 0, -1, false, { "SELECT * FROM CUSTOMERS" })
  vim.api.nvim_set_current_buf(first_buf)
  local client_id = vim.lsp.start(config, { bufnr = first_buf })
  if not client_id then
    fail("vim.lsp.start did not start fixture client")
  end
  local client = vim.lsp.get_client_by_id(client_id)
  if not client then
    fail("cannot look up started fixture client")
  end

  wait_for("LSP initialize", function()
    return client.initialized == true
  end)
  wait_for("initial metadata-ready status", function()
    local result = request(client, "workspace/executeCommand", { command = "sqls.showMetadataStatus" }, first_buf)
    return result.connectionState == "ready" and result.settled == true and result.degraded ~= true
  end)

  -- Completion DTO: table and selectable procedure labels/kinds/docs.
  local completion_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "SELECT * FROM ")
  local completion = request(client, "textDocument/completion", text_document_position(completion_buf, 0, 14), completion_buf)
  if type(completion) ~= "table" or type(completion.items) ~= "table" then
    fail("completion did not return a CompletionList: " .. vim.inspect(completion))
  end
  local table_item, procedure_item
  for _, item in ipairs(completion.items) do
    if item.label == "CUSTOMERS" then table_item = item end
    if item.label == "FIND_CUSTOMER" then procedure_item = item end
  end
  local table_documentation = table_item and table_item.documentation
  if type(table_documentation) == "table" then
    table_documentation = table_documentation.value
  end
  if not table_item or table_item.kind ~= 7 or not contains(table_documentation, "CUSTOMER_ID") then
    fail("CUSTOMERS completion DTO mismatch: " .. vim.inspect(table_item))
  end
  if not procedure_item or procedure_item.kind ~= 2 then
    fail("FIND_CUSTOMER completion DTO mismatch: " .. vim.inspect(procedure_item))
  end

  -- Hover DTO and exact half-open relation range.
  local hover_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "SELECT * FROM CUSTOMERS")
  local hover = request(client, "textDocument/hover", text_document_position(hover_buf, 0, 20), hover_buf)
  if not hover or not contains(vim.inspect(hover.contents), "CUSTOMERS") then
    fail("hover did not identify CUSTOMERS: " .. vim.inspect(hover))
  end
  assert_equal(hover.range, { start = position(0, 14), ["end"] = position(0, 23) }, "hover range")
  for _, column in ipairs({ "CUSTOMER_ID", "NAME", "EMAIL" }) do
    if not contains(vim.inspect(hover.contents), column) then
      fail("hover omitted column " .. column .. ": " .. vim.inspect(hover.contents))
    end
  end

  -- Procedure signature DTO.
  local signature_text = "EXECUTE PROCEDURE FIND_CUSTOMER("
  local signature_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", signature_text)
  local signature = request(client, "textDocument/signatureHelp", text_document_position(signature_buf, 0, #signature_text), signature_buf)
  if not signature or not signature.signatures or #signature.signatures < 1 then
    fail("signatureHelp returned no signature: " .. vim.inspect(signature))
  end
  local sig = signature.signatures[1]
  local parameter = sig.parameters and sig.parameters[1]
  if not contains(sig.label, "P_CUSTOMER_ID") or not parameter or parameter.label ~= "P_CUSTOMER_ID"
      or not contains(parameter.documentation, "INTEGER") or signature.activeParameter ~= 0 then
    fail("signatureHelp DTO mismatch: " .. vim.inspect(signature))
  end

  -- Definition must follow the generated URI, verify its banner/body and the
  -- body-relative range without assuming a temp path or banner line count.
  local navigation_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "SELECT * FROM CUSTOMERS")
  local definition = request(client, "textDocument/definition", text_document_position(navigation_buf, 0, 20), navigation_buf)
  local location = type(definition) == "table" and definition[1] or nil
  if not location or type(location.uri) ~= "string" or not location.uri:match("^file:") or location.uri == uri_for(navigation_buf) then
    local status = request(client, "workspace/executeCommand", { command = "sqls.showMetadataStatus" }, navigation_buf)
    local at_start = request(client, "textDocument/definition", text_document_position(navigation_buf, 0, 14), navigation_buf)
    fail("definition location must be a distinct generated file URI; raw DTO=" .. vim.inspect(definition)
      .. "; at-token-start DTO=" .. vim.inspect(at_start) .. "; metadata status=" .. vim.inspect(status))
  end
  local snapshot_text = read_file(vim.uri_to_fname(location.uri))
  local ddl_line
  local ddl_body_line
  local body_offset = 0
  for line in (snapshot_text .. "\n"):gmatch("(.-)\n") do
    if line:find("CREATE TABLE CUSTOMERS", 1, true) then
      ddl_line = body_offset
      ddl_body_line = line
      break
    end
    body_offset = body_offset + 1
  end
  if not ddl_line then
    fail("definition snapshot has no CREATE TABLE CUSTOMERS body: " .. snapshot_text)
  end
  if not contains(snapshot_text, "-- sqls: read-only snapshot of InterBase TABLE \"CUSTOMERS\"")
      or not contains(snapshot_text, "-- Editing this file does not change the database.") then
    fail("definition snapshot is missing its read-only source banner: " .. snapshot_text)
  end
  assert_equal(location.range.start.line, ddl_line, "definition body-relative start line")
  assert_equal(location.range["end"].line, ddl_line, "definition body-relative end line")
  if location.range.start.character < 0 or location.range["end"].character < location.range.start.character
      or ddl_body_line:sub(location.range.start.character + 1, location.range["end"].character) ~= "CUSTOMERS" then
    fail("definition returned invalid UTF-16 range: " .. vim.inspect(location.range))
  end

  -- Relation names are not local references.
  local relation_refs = request(client, "textDocument/references", {
    textDocument = text_document(navigation_buf), position = position(0, 20), context = { includeDeclaration = true },
  }, navigation_buf)
  assert_equal(relation_refs, vim.empty_dict and vim.empty_dict() or {}, "relation references")

  -- Procedure-local references and rename ranges include UTF-16 after 😀.
  local local_text = "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv = 0;\nv = :v + CHAR_LENGTH('😀') + v; -- v\nSELECT 'v' FROM t;\nEND"
  local local_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", local_text)
  local local_uri = uri_for(local_buf)
  local local_params = { textDocument = { uri = local_uri }, position = position(4, 29), context = { includeDeclaration = true } }
  local references = request(client, "textDocument/references", local_params, local_buf)
  local want_ranges = {
    { start = position(1, 17), ["end"] = position(1, 18) },
    { start = position(3, 0), ["end"] = position(3, 1) },
    { start = position(4, 0), ["end"] = position(4, 1) },
    { start = position(4, 5), ["end"] = position(4, 6) },
    { start = position(4, 29), ["end"] = position(4, 30) },
  }
  if type(references) ~= "table" or #references ~= 5 then
    fail("procedure-local references count = " .. vim.inspect(references) .. "; want five ranges")
  end
  for i, want in ipairs(want_ranges) do
    assert_equal(references[i].range, want, "procedure reference " .. i .. " range")
    assert_equal(references[i].uri, local_uri, "procedure reference " .. i .. " URI")
  end
  local rename = request(client, "textDocument/rename", {
    textDocument = { uri = local_uri }, position = position(4, 29), newName = "V_AMOUNT",
  }, local_buf)
  local edits = rename and rename.changes and rename.changes[local_uri]
  if type(edits) ~= "table" or #edits ~= 5 then
    fail("rename expected five edits for exact source URI: " .. vim.inspect(rename))
  end
  for i, want in ipairs(want_ranges) do
    assert_equal(edits[i].range, want, "rename edit " .. i .. " range")
    assert_equal(edits[i].newText, "V_AMOUNT", "rename edit " .. i .. " text")
  end

  -- Formatting DTO must include an edit and format the SQL conventionally.
  local format_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "select * from customers")
  local formatting = request(client, "textDocument/formatting", {
    textDocument = text_document(format_buf), options = { tabSize = 2, insertSpaces = true },
  }, format_buf)
  local original_format_text = "select * from customers"
  local dummy_noop_accepted = pcall(assert_formatting_edits, {
    { range = { start = position(0, 0), ["end"] = position(0, 0) }, newText = original_format_text },
  }, original_format_text)
  if dummy_noop_accepted then
    fail("formatting assertion accepted a deliberately weakened no-op edit")
  end
  local dummy_irrelevant_accepted = pcall(assert_formatting_edits, {
    { range = { start = position(0, 0), ["end"] = position(1, 0) }, newText = "SELECT unrelated text" },
  }, original_format_text)
  if dummy_irrelevant_accepted then
    fail("formatting assertion accepted a deliberately weakened irrelevant edit")
  end
  assert_formatting_edits(formatting, original_format_text)

  -- Document-symbol LSP adapter DTO: actual local variable and exact ranges.
  local symbols_text = "CREATE PROCEDURE P AS DECLARE VARIABLE X INTEGER; BEGIN X = 1; END"
  local symbols_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", symbols_text)
  local symbols = request(client, "textDocument/documentSymbol", { textDocument = text_document(symbols_buf) }, symbols_buf)
  if type(symbols) ~= "table" or #symbols ~= 1 then
    fail("documentSymbol DTO expected one symbol: " .. vim.inspect(symbols))
  end
  assert_equal(symbols[1].name, "X", "document symbol name")
  assert_equal(symbols[1].kind, 13, "document symbol kind")
  assert_equal(symbols[1].range, { start = position(0, 0), ["end"] = position(0, 66) }, "document symbol range")
  assert_equal(symbols[1].selectionRange, { start = position(0, 39), ["end"] = position(0, 40) }, "document symbol selection range")

  -- Diagnostics are asynchronous/coalesced. Validate the latest document
  -- version after an invalid edit and after restoring valid SQL.
  local diagnostic_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "SELECT * FROM CUSTOMERS")
  local diagnostic_uri = uri_for(diagnostic_buf)
  wait_for("initial diagnostics document open", function()
    return vim.api.nvim_buf_is_loaded(diagnostic_buf)
  end)
  vim.api.nvim_buf_set_lines(diagnostic_buf, 0, -1, false, {
    "CREATE PROCEDURE P AS DECLARE VARIABLE LOCAL_VALUE VARCHAR(10); BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END",
  })
  local buffer_versions = require("vim.lsp.util").buf_versions
  wait_for("Neovim invalid-edit document version", function()
    return type(buffer_versions[diagnostic_buf]) == "number" and buffer_versions[diagnostic_buf] > 0
  end)
  local invalid_version = buffer_versions[diagnostic_buf]
  wait_for("invalid-edit diagnostic publication", function()
    local list = (diagnostics_by_uri[diagnostic_uri] or {})[invalid_version]
    return list ~= nil and #list > 0
  end)
  vim.api.nvim_buf_set_lines(diagnostic_buf, 0, -1, false, { "SELECT * FROM CUSTOMERS" })
  wait_for("Neovim latest valid document version", function()
    return type(buffer_versions[diagnostic_buf]) == "number" and buffer_versions[diagnostic_buf] > invalid_version
  end)
  local valid_version = buffer_versions[diagnostic_buf]
  if valid_version <= invalid_version then
    fail("diagnostic buffer version did not advance after valid edit")
  end
  wait_for("empty diagnostics for latest valid document version", function()
    local latest = diagnostics_by_uri[diagnostic_uri] or {}
    return latest[valid_version] ~= nil and #latest[valid_version] == 0
  end)

  local function metadata_status()
    return request(client, "workspace/executeCommand", { command = "sqls.showMetadataStatus" }, first_buf)
  end
  local function query_command(command, target_buf)
    local result = request(client, "workspace/executeCommand", { command = command, arguments = { uri_for(target_buf) } }, target_buf)
    if type(result) ~= "string" then
      fail(command .. " returned non-string result: " .. vim.inspect(result))
    end
    return result
  end

  local query_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", "SELECT * FROM CUSTOMERS")
  local query_result = query_command("executeQuery", query_buf)
  if not contains(query_result, "42") then fail("executeQuery result omitted fixture value 42: " .. query_result) end
  local explain_result = query_command("explainQuery", query_buf)
  if not contains(explain_result, "PLAN (CUSTOMERS NATURAL)") then fail("explainQuery result mismatch: " .. explain_result) end

  -- Switch to new, confirm readiness and new target metadata/DDL, then reselect
  -- the same target to exercise SQLS's explicit refresh path.
  request(client, "workspace/executeCommand", { command = "switchConnections", arguments = { "new" } }, first_buf)
  wait_for("new target metadata readiness", function()
    local status = metadata_status()
    return status.connectionState == "ready" and status.settled == true and status.degraded ~= true
      and (status.generation or 0) > 0
  end)
  local new_hover = request(client, "textDocument/hover", text_document_position(hover_buf, 0, 20), hover_buf)
  if not contains(vim.inspect(new_hover.contents), "VARCHAR(100)") then
    fail("new target hover did not expose NAME VARCHAR(100): " .. vim.inspect(new_hover))
  end
  local new_definition = request(client, "textDocument/definition", text_document_position(navigation_buf, 0, 20), navigation_buf)
  local new_loc = type(new_definition) == "table" and new_definition[1] or nil
  if not new_loc or not contains(read_file(vim.uri_to_fname(new_loc.uri)), "NAME VARCHAR(100)") then
    fail("new target definition DDL did not expose VARCHAR(100): " .. vim.inspect(new_definition))
  end
  local before_reselect = metadata_status()
  if before_reselect.connectionState ~= "ready" or before_reselect.settled ~= true or before_reselect.degraded == true then
    fail("new-target metadata was not settled before reselect: " .. vim.inspect(before_reselect))
  end
  local no_advance_accepted = pcall(assert_reselected_metadata, before_reselect, vim.deepcopy(before_reselect))
  if no_advance_accepted then
    fail("reselection assertion accepted an unchanged metadata status")
  end
  request(client, "workspace/executeCommand", { command = "switchConnections", arguments = { "new" } }, first_buf)
  local after_reselect
  wait_for("reselected target refresh readiness", function()
    after_reselect = metadata_status()
    assert_reselected_metadata(before_reselect, after_reselect)
    return true
  end)
  print(string.format(
    "new-target reselect metadata: connectionGeneration %d -> %d; generation %d -> %d; revision %d -> %d",
    before_reselect.connectionGeneration, after_reselect.connectionGeneration,
    before_reselect.generation, after_reselect.generation, before_reselect.revision, after_reselect.revision
  ))
  new_hover = request(client, "textDocument/hover", text_document_position(hover_buf, 0, 20), hover_buf)
  local reselected_hover_text = vim.inspect(new_hover.contents)
  if not contains(reselected_hover_text, "VARCHAR(100)") or contains(reselected_hover_text, "VARCHAR(80)") then
    fail("reselected target lost NAME VARCHAR(100): " .. vim.inspect(new_hover))
  end
  local reselected_definition = request(client, "textDocument/definition", text_document_position(navigation_buf, 0, 20), navigation_buf)
  local reselected_location = type(reselected_definition) == "table" and reselected_definition[1] or nil
  local reselected_ddl = reselected_location and read_file(vim.uri_to_fname(reselected_location.uri)) or ""
  if not contains(reselected_ddl, "NAME VARCHAR(100)") or contains(reselected_ddl, "NAME VARCHAR(80)") then
    fail("reselected target definition accepted stale old-target DDL: " .. vim.inspect(reselected_definition)
      .. "; DDL=" .. reselected_ddl)
  end

  -- Cancel only after the framed query-entry notification proves the exact SQL
  -- is inside the fake repository's gate.
  local gated_sql = "SELECT SQLS_FIXTURE_WAIT FROM CUSTOMERS"
  local gated_buf = open_buffer(client_id, vim.fn.tempname() .. ".sql", gated_sql)
  local async_done, async_result, async_error = false, nil, nil
  local sent, request_id = client:request("workspace/executeCommand", {
    command = "executeQuery", arguments = { uri_for(gated_buf) },
  }, function(err, result)
    async_error, async_result, async_done = err, result, true
  end, gated_buf)
  if not sent or request_id == nil then fail("failed to start async executeQuery request: " .. vim.inspect({ sent, request_id })) end
  wait_for("framed fixture query-entry notification", function()
    for _, entry in ipairs(fixture_entries) do
      if entry.marker == "SQLS_FIXTURE_WAIT" and entry.sql == gated_sql and entry.target == "fixture://new" then
        return true
      end
    end
    return false
  end)
  client:notify("$/cancelRequest", { id = request_id })
  wait_for("correlated cancellation response", function() return async_done end, 8000)
  if async_error then
    fail("cancelled executeQuery returned RPC error instead of command result: " .. vim.inspect(async_error))
  end
  if type(async_result) ~= "string" or not contains(async_result:lower(), "cancel") then
    fail("cancelled executeQuery result did not report cancellation: " .. vim.inspect(async_result))
  end
  local after_cancel = query_command("executeQuery", query_buf)
  if not contains(after_cancel, "42") then fail("ordinary query after cancellation failed: " .. after_cancel) end

  -- Give SQLS a full graceful shutdown window; Neovim force-stops only if
  -- the server fails to acknowledge shutdown before this bounded timeout.
  client:stop(5000)
  wait_for("fixture child shutdown and Neovim client removal", function()
    return vim.lsp.get_client_by_id(client_id) == nil
  end, 8000)
  if #unexpected > 0 then
    fail("unexpected LSP notification/client errors: " .. table.concat(unexpected, "\n"))
  end
  vim.lsp.handlers["textDocument/publishDiagnostics"] = original_diagnostics_handler
  print("SQLS Neovim shared-core acceptance: PASS")
end

local ok, err = xpcall(run, debug.traceback)
if not ok then
  local clients = vim.lsp.get_clients({ name = "sqls-neovim-shared-core-acceptance" })
  for _, client in ipairs(clients) do
    pcall(function() client:stop(true) end)
  end
  vim.lsp.handlers["textDocument/publishDiagnostics"] = nil
  io.stderr:write(tostring(err) .. "\n")
  vim.cmd("cquit 1")
end
