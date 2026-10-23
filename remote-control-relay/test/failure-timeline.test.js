import assert from "node:assert/strict";
import test from "node:test";
import { failureTimeline, redactDiagnosticText } from "../failure-diagnostics.js";
import { validateBehaviorEventShape } from "../behavior-telemetry-schema.js";

const event = (index, extra = {}) => ({event_id:`timeline_event_${index}`,device_id:"device_fixture",
  collection_period_id:"period_fixture",turn_id:"turn_fixture",attempt_id:"attempt_fixture",
  event_name:"tool_call_completed",occurred_at:1000+index*100,...extra});

test("long tasks retain the incident and nearby events as well as the late task terminal", () => {
  const events=Array.from({length:120},(_,index)=>event(index));
  const incident=events[20];
  Object.assign(incident,{diagnostic_id:incident.event_id,diagnostic_exit_code:3221225477,
    diagnostic_code:"process_failed",tool_name:"bash",command:"PRIVATE_COMMAND",stderr:"PRIVATE_STDERR"});
  events[119].event_name="task_finished";events[119].status="success";
  const result=failureTimeline(events.toReversed(),incident);
  assert.equal(result.timeline.length,32);
  assert(result.timeline.some(e=>e.occurred_at===incident.occurred_at&&e.diagnostic_exit_code===3221225477));
  assert(result.timeline.some(e=>e.occurred_at===events[19].occurred_at));
  assert(result.timeline.some(e=>e.occurred_at===events[21].occurred_at));
  assert.equal(result.timeline.at(-1).status,"success");
  assert.deepEqual(result.timeline_context,{total_events:120,returned_events:32,truncated:true,
    incident_included:true,task_terminal_included:true});
  assert(!JSON.stringify(result).includes("PRIVATE_"));
  assert(!JSON.stringify(result.timeline).includes("turn_fixture"));
  assert.deepEqual(result.timeline.map(e=>e.occurred_at),result.timeline.map(e=>e.occurred_at).toSorted((a,b)=>a-b));
});

test("window covers beginning, end, exact capacity and missing terminal without fabricating evidence", () => {
  for(const length of [1,10,32,33,100])for(const index of [0,length-1]) {
    const events=Array.from({length},(_,i)=>event(i));
    const result=failureTimeline(events,events[index]);
    assert.equal(result.timeline.length,Math.min(length,32));
    assert.equal(result.timeline_context.truncated,length>32);
    assert.equal(result.timeline_context.incident_included,true);
    assert.equal(result.timeline_context.task_terminal_included,false);
  }
  const incident=event(1);
  assert.deepEqual(failureTimeline([],incident).timeline,[{event_name:incident.event_name,occurred_at:incident.occurred_at}]);
});

test("correlation excludes another turn, device or consent period even when other IDs coincide", () => {
  const incident=event(20,{operation_id:"shared_operation"});
  const events=[incident,event(21),event(22,{turn_id:"other_turn",event_name:"task_finished",status:"failed"}),
    event(23,{device_id:"other_device"}),event(24,{collection_period_id:"other_period"}),
    event(25,{attempt_id:"other_attempt"})];
  assert.equal(failureTimeline(events,incident).timeline.length,2);
  const attempt={...incident,turn_id:undefined};
  assert.equal(failureTimeline([attempt,event(21,{turn_id:undefined})],attempt).timeline.length,2);
  const operation={...attempt,attempt_id:undefined};
  assert.equal(failureTimeline([operation,event(21,{operation_id:"shared_operation"})],operation).timeline.length,2);
  const unbound={...operation,operation_id:undefined};
  assert.equal(failureTimeline(events,unbound).timeline.length,1);
  const legacy={...incident,collection_period_id:undefined};
  assert.equal(failureTimeline([legacy,event(21)],legacy).timeline.length,1);
});

test("Windows process status keeps all 32 bits through schema validation and text redaction", () => {
  const witness={schema_version:1,event_id:"beh_exit_diagnostic_fixture_1",event_name:"tool_call_completed",event_source:"client",
    occurred_at:Date.now(),collection_period_id:"fixture_collection_period",producer:"pinvou_agent",producer_version:"0.10.14",
    app_version:"0.10.14",platform:"windows",arch:"x86_64",channel:"stable",locale:"en",
    session_id:"session_exit_fixture",turn_id:"turn_exit_fixture",
    operation_id:"operation_exit_fixture",attempt_index:1,tool_key:"shell",tool_type:"cli",tool_name:"bash",
    success:false,duration_ms:1765,error_category:"tool_error",stage:"tool",
    diagnostic_id:"beh_exit_diagnostic_fixture_1",diagnostic_trace_id:"a".repeat(64),diagnostic_code:"process_failed",
    diagnostic_component:"tool",diagnostic_site:"tool_result"};
  for(const code of [-2147483648,0,7,3221225477,4294967295]) {
    const result=validateBehaviorEventShape({...witness,diagnostic_exit_code:code});
    assert.equal(result.ok,true,JSON.stringify(result));
    assert.equal(JSON.parse(redactDiagnosticText(JSON.stringify({exit_code:code})) ).exit_code,code);
  }
  for(const code of [-2147483649,4294967296,"7",1.5]) {
    assert.equal(validateBehaviorEventShape({...witness,diagnostic_exit_code:code}).ok,false);
    assert.notEqual(JSON.parse(redactDiagnosticText(JSON.stringify({exit_code:code}))).exit_code,code);
  }
});

test("remote execution keeps the controlled authoritative terminal without mixing receipt or identity boundaries", () => {
  const key="b".repeat(64);
  const incident=event(20,{execution_task_key:key,diagnostic_code:"process_failed"});
  const terminal=event(100,{event_name:"remote_control_event",turn_id:undefined,attempt_id:undefined,
    remote_task_key:key,remote_role:"controlled",remote_stage:"task_terminal",remote_task_status:"success"});
  const events=Array.from({length:100},(_,i)=>event(i,{execution_task_key:key}));
  events[20]=incident;
  for(const extra of [
    {device_id:"another_device"},{collection_period_id:"another_period"},
    {remote_role:"controller"},{remote_stage:"result_received"},
    {remote_task_key:"c".repeat(64)},
  ]) {
    const result=failureTimeline([...events,{...terminal,...extra}],incident);
    assert.equal(result.timeline_context.task_terminal_included,false);
  }
  const result=failureTimeline([...events,terminal],incident);
  assert.equal(result.timeline.length,32);
  assert.equal(result.timeline_context.task_terminal_included,true);
  assert.equal(result.timeline.at(-1).remote_task_status,"success");
  assert(result.timeline.some(item=>item.diagnostic_code==="process_failed"));
  assert(!JSON.stringify(result).includes(key));
  const unknown=failureTimeline([incident,{...terminal,remote_task_status:"unknown"}],incident);
  assert.equal(unknown.timeline_context.task_terminal_included,false);
  assert.equal(unknown.timeline.at(-1).remote_task_status,"unknown");
});
