import assert from "node:assert/strict";
import test from "node:test";
import { unifiedAppTraceRows } from "./unified-app-trace.ts";

// Measured phases stay ordered, including zero-duration stages, without inventing unexecuted output validation.
test("app trace preserves recorded stages and missing evidence", () => {
  const rows = unifiedAppTraceRows({latency_ms:43,timings:[{name:"unified_app_execute",duration_ms:40},{name:"unified_app_input_validation",duration_ms:0},{name:"provider_http",duration_ms:20}]});
  assert.deepEqual(rows.map(row=>row.name),["input_validation","execute"]);
  assert.equal(rows[0].duration_ms,0);
  assert.equal(rows[1].duration_ms,40);
  assert.deepEqual(unifiedAppTraceRows({latency_ms:43}),[]);
});

// Malformed timing evidence cannot overflow the visual bar or masquerade as an observed phase.
test("app trace rejects invalid durations and bounds bars", () => {
  const rows=unifiedAppTraceRows({latency_ms:0,timings:[{name:"unified_app_execute",duration_ms:2},{name:"unified_app_initialization",duration_ms:-1},{name:"unified_app_output_validation",duration_ms:NaN}]});
  assert.equal(rows.length,1);
  assert.equal(rows[0].percent,100);
});
