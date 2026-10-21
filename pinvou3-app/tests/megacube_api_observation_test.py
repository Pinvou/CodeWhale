"""Failed local HTTP observations must not invent empty inventories or task acceptance."""
import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest
from unittest.mock import patch

SERVER = Path(__file__).resolve().parents[1] / 'src-tauri/resources/common/bundle/mcp-servers/megacube_admin_server.py'
SPEC = importlib.util.spec_from_file_location('api_observation_server', SERVER)
server = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(server)

class Fixture:
    def __init__(self, routes):
        self.routes, self.calls, self.stop = routes, [], threading.Event()
    def __enter__(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='megacube-api-observation-')
        self.path = str(Path(self.temporary.name)/'api.sock')
        self.env = patch.dict(os.environ, {'PINVOU3_HOME':self.temporary.name, 'CMSD_SOCKET':self.path})
        self.env.__enter__()
        self.listener=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
        self.listener.bind(self.path); self.listener.listen(4); self.listener.settimeout(.1)
        self.thread=threading.Thread(target=self.run,daemon=True); self.thread.start()
        return self
    def run(self):
        while not self.stop.is_set():
            try: conn,_=self.listener.accept()
            except socket.timeout: continue
            with conn:
                raw=b''
                while b'\r\n\r\n' not in raw: raw+=conn.recv(65536)
                head,_,body=raw.partition(b'\r\n\r\n')
                length=next((int(line.split(b':',1)[1]) for line in head.split(b'\r\n') if line.lower().startswith(b'content-length:')),0)
                while len(body)<length: body+=conn.recv(65536)
                method,path,_=head.split(b'\r\n',1)[0].decode().split(' ')
                self.calls.append((method,path))
                status,value=self.routes[path]
                body=value.encode() if isinstance(value,str) else json.dumps(value).encode()
                conn.sendall(('HTTP/1.1 %s Fixture\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n' % (status,len(body))).encode()+body)
    def __exit__(self,*args):
        self.stop.set(); self.thread.join(timeout=2); self.listener.close()
        self.env.__exit__(*args); self.temporary.cleanup()

ERROR={'error':{'code':'store_selection_unavailable','message':'store inventory unavailable'}}
STATUS={'service':'cmsd','version':'0.1.0','uptime_s':1,'active_model':'old','models':[],'degraded':[]}
MODELS={'models':[{'card':'target','status':'stopped','managed':False}], 'store_bindings':{},'active_model':None}

class ApiObservationTests(unittest.TestCase):
    def assert_unknown(self,result,code):
        self.assertEqual(result['data']['observation'],'unconfirmed')
        self.assertEqual(result['data']['error_code'],code)
        self.assertNotIn('共 0 个模型',result['summary'])
        self.assertNotIn('已下发',result['summary'])
    def test_inventory_503_keeps_error_and_independent_records(self):
        with Fixture({'/v1/models':(503,ERROR)}):
            result=server._tool_models()
        self.assert_unknown(result,'store_selection_unavailable')
        self.assertEqual(result['data']['error'],ERROR['error'])
        self.assertEqual(result['data']['http_status'],503)
        self.assertNotIn('models',result['data'])
        self.assertEqual(result['data']['external_artifacts'],[])
    def test_read_tools_reject_http_errors(self):
        for path,tool in [('/v1/status',server._tool_status),('/v1/models',server._tool_models),
                          ('/v1/capacity',server._tool_capacity),
                          ('/v1/models/model/logs?tail=100',lambda:server._tool_logs({'card':'model'}))]:
            with self.subTest(path=path), Fixture({path:(403,{'error':{'code':'permission_denied','message':'access denied'}})}):
                result=tool()
                self.assert_unknown(result,'permission_denied')
                self.assertEqual(result['data']['error']['message'],'access denied')
    def test_read_tools_reject_bad_json_and_missing_shapes(self):
        for path,tool in [('/v1/status',server._tool_status),('/v1/models',server._tool_models),
                          ('/v1/capacity',server._tool_capacity),
                          ('/v1/models/model/logs?tail=100',lambda:server._tool_logs({'card':'model'}))]:
            for value in ('{','[]','null','{}'):
                with self.subTest(path=path,value=value), Fixture({path:(200,value)}):
                    self.assert_unknown(tool(),'CMSD_INVALID_RESPONSE')
    def test_real_empty_inventory_remains_confirmed(self):
        with Fixture({'/v1/models':(200,{'models':[],'store_bindings':{},'active_model':None})}):
            result=server._tool_models()
        self.assertIn('共 0 个模型',result['summary'])
        self.assertEqual(result['data']['models'],[])
        self.assertTrue(result['data']['cmsd_available'])
    def test_status_requires_actual_minimum_facts(self):
        invalid = [{'version':'x'}]
        invalid += [{key:value for key,value in STATUS.items() if key!=missing} for missing in STATUS]
        invalid += [dict(STATUS,**{field:value}) for field,value in (
            ('service','other'),('version',' '),('uptime_s',True),('uptime_s',-1),
            ('active_model',False),('models',[{}]),('degraded',[{}]),
            ('degraded',[{'card':'model','reason':False}]))]
        for value in invalid:
            with self.subTest(value=value),Fixture({'/v1/status':(200,value)}):
                self.assert_unknown(server._tool_status(),'CMSD_INVALID_RESPONSE')
    def test_status_confirms_only_observed_facts(self):
        with Fixture({'/v1/status':(200,dict(STATUS,active_model=None))}):
            result=server._tool_status()
        self.assertTrue(result['data']['cmsd_available'])
        self.assertEqual(result['data']['models'],[])
        self.assertIsNone(result['data']['active_model'])
    def test_model_rows_require_identity_status_and_ownership_fact(self):
        for row in ({},{'card':'m'},{'card':'m','status':'stopped'},
                    {'card':'','status':'stopped','managed':False},
                    {'card':'m','status':'','managed':False},
                    {'card':'m','status':None,'managed':False},
                    {'card':'m','status':'stopped','managed':'true'}):
            with self.subTest(row=row),Fixture({'/v1/models':(200,dict(MODELS,models=[row]))}):
                self.assert_unknown(server._tool_models(),'CMSD_INVALID_RESPONSE')
        with Fixture({'/v1/models':(200,{'models':[]})}):
            self.assert_unknown(server._tool_models(),'CMSD_INVALID_RESPONSE')
    def test_optional_model_fields_are_not_required(self):
        with Fixture({'/v1/models':(200,MODELS)}):
            result=server._tool_models()
        self.assertEqual(result['data']['models'],MODELS['models'])
        self.assertIn('共 1 个模型',result['summary'])
    def test_task_rows_require_nonempty_identity_and_status(self):
        for row in ({},{'id':'task-one'},{'id':'','status':'running'},
                    {'id':'task-one','status':''},{'id':5,'status':'running'}):
            with self.subTest(row=row),Fixture({'/v1/tasks':(200,{'tasks':[row]})}):
                result=server._tool_task({})
            self.assertEqual(result['data']['cmsd_task_observation'],'unconfirmed')
            self.assertEqual(result['data']['observation_error']['code'],'CMSD_INVALID_RESPONSE')
            self.assertNotIn('管理面没有任务记录',result['summary'])
    def test_task_detail_requires_exact_identity_and_nonempty_status(self):
        for row in ({},{'id':'task-one'},{'id':'','status':'running'},
                    {'id':'task-other','status':'succeeded'},{'id':'task-one','status':''}):
            with self.subTest(row=row),Fixture({'/v1/tasks/task-one':(200,row)}):
                result=server._tool_task({'task_id':'task-one','wait_seconds':0})
            self.assertEqual(result['data']['status'],'unknown')
            self.assertEqual(result['data']['observation_error_code'],'CMSD_INVALID_RESPONSE')
    def test_future_model_state_is_explicit_unknown_without_hiding_identity(self):
        row={'card':'target','status':'future-state','managed':False,'weights_complete':True}
        with Fixture({'/v1/models':(200,dict(MODELS,models=[row]))}):
            result=server._tool_models()
        self.assertIn('共 1 个模型',result['summary'])
        self.assertIn('尚未核实',result['summary'])
        observed=result['data']['models'][0]
        self.assertEqual(observed['card'],'target')
        self.assertEqual(observed['status'],'unknown')
        self.assertEqual(observed['reported_status'],'future-state')
        self.assertEqual(server._local_state(observed,None),'unknown')
        with Fixture({'/v1/models':(200,dict(MODELS,models=[row])),'/v1/status':(200,STATUS)}),patch.object(server,'_fetch',return_value=(200,b'[{"id":"target"}]')):
            catalog=server._tool_store_models()
        self.assertEqual(catalog['data']['models'][0]['local_state'],'unknown')
        self.assertIn('不能据此重新安装',catalog['next'])
        with Fixture({'/v1/status':(200,dict(STATUS,models=[row]))}):
            result=server._tool_status()
        self.assertIn('运行状态尚未核实 1 个',result['summary'])
    def test_future_task_state_is_unknown_and_keeps_observed_receipt(self):
        row={'id':'task-one','status':'future-state'}
        with Fixture({'/v1/tasks/task-one':(200,row)}):
            result=server._tool_task({'task_id':'task-one','wait_seconds':0})
        self.assertEqual(result['data']['id'],'task-one')
        self.assertEqual(result['data']['status'],'unknown')
        self.assertEqual(result['data']['reported_status'],'future-state')
        self.assertIn('状态尚未核实',result['next'])
        self.assertNotIn('cube_resume_task',result['next'])
        with Fixture({'/v1/tasks':(200,{'tasks':[row]})}):
            result=server._tool_task({})
        self.assertEqual(result['data']['cmsd_task_observation'],'available')
        self.assertEqual(result['data']['tasks'][0]['status'],'unknown')
    def test_switch_plan_does_not_invent_budget_from_failed_capacity(self):
        with Fixture({'/v1/status':(200,STATUS),'/v1/models':(200,MODELS),'/v1/capacity?card=target':(503,ERROR)}):
            result=server._tool_plan_switch({'card':'target'})
        self.assert_unknown(result,'store_selection_unavailable')
        self.assertNotIn('budget_ok',result['data'])
        self.assertNotIn('cube_switch_model',result['next'])
    def test_switch_plan_requires_status_observation(self):
        with Fixture({'/v1/status':(503,ERROR),'/v1/models':(200,MODELS)}) as fixture:
            result=server._tool_plan_switch({'card':'target'})
        self.assert_unknown(result,'store_selection_unavailable')
        self.assertFalse(any(path.startswith('/v1/capacity') for _,path in fixture.calls))
    def test_task_index_unknown_remains_unknown(self):
        with Fixture({'/v1/tasks':(503,ERROR)}):
            result=server._tool_task({})
        self.assertEqual(result['data']['cmsd_task_observation'],'unconfirmed')
        self.assertEqual(result['data']['observation_error'],ERROR['error'])
        self.assertNotIn('管理面没有任务记录',result['summary'])
    def test_task_lookup_retains_daemon_reason(self):
        with Fixture({'/v1/tasks/task-one':(503,ERROR)}):
            result=server._tool_task({'task_id':'task-one','wait_seconds':0})
        self.assertEqual(result['data']['status'],'unknown')
        self.assertEqual(result['data']['observation_error_code'],'store_selection_unavailable')
    def test_observed_task_execution_failure_is_preserved(self):
        for error in ('engine failed', {'code':'engine_failed','message':'engine failed'}):
            with self.subTest(error=error), Fixture({'/v1/tasks/task-one':(200,{'id':'task-one','status':'failed','kind':'enable','error':error})}):
                result=server._tool_task({'task_id':'task-one','wait_seconds':0})
            self.assertEqual(result['data']['status'],'failed')
            self.assertEqual(result['data']['error'],error)
            self.assertNotIn('observation',result['data'])
    def test_non2xx_task_shape_is_not_accepted_or_replayed(self):
        with Fixture({'/v1/models/model/enable':(503,{'task_id':'task-one','status':'running'})}) as fixture:
            result=server._tool_start({'card':'model'})
        self.assert_unknown(result,'CMSD_HTTP_503')
        self.assertEqual(fixture.calls,[('POST','/v1/models/model/enable')])
    def test_malformed_or_missing_management_receipt_is_not_accepted(self):
        for value in ('{','[]','null','{}'):
            with self.subTest(value=value),Fixture({'/v1/models/model/enable':(202,value)}) as fixture:
                result=server._tool_start({'card':'model'})
                self.assertEqual(result['data']['observation'],'unconfirmed')
                self.assertNotIn('已下发',result['summary'])
                self.assertIn('不自动重发',result['next'])
                self.assertEqual(len(fixture.calls),1)
    def test_real_rejection_keeps_code_without_claiming_acceptance(self):
        with Fixture({'/v1/models/model/enable':(409,ERROR)}):
            result=server._tool_start({'card':'model'})
        self.assertIn('被拒',result['summary'])
        self.assertEqual(result['data']['error'],ERROR['error'])
        self.assertEqual(result['data']['http_status'],409)
        self.assertNotIn('task_id',result['data'])
    def test_successful_receipt_with_null_error_is_preserved(self):
        with Fixture({'/v1/models/model/enable':(202,{'task_id':'task-one','status':'running','error':None})}):
            result=server._tool_start({'card':'model'})
        self.assertIn('已下发',result['summary'])
        self.assertEqual(result['data']['task_id'],'task-one')
    def test_mcp_marks_failed_observations_as_errors(self):
        for name,path,status,value,args in [
            ('cube_list_models','/v1/models',503,ERROR,{}),
            ('cube_start_model','/v1/models/model/enable',202,'{',{'card':'model'}),
        ]:
            with self.subTest(name=name),Fixture({path:(status,value)}):
                output=io.StringIO()
                with contextlib.redirect_stdout(output):
                    server._handle({'id':1,'method':'tools/call','params':{'name':name,'arguments':args}})
                result=json.loads(output.getvalue())['result']
            self.assertTrue(result['isError'])
    def test_missing_socket_preserves_independent_inventory_without_fake_empty_models(self):
        with tempfile.TemporaryDirectory() as temp, patch.dict(os.environ,{'PINVOU3_HOME':temp,'CMSD_SOCKET':temp+'/absent.sock'}):
            result=server._tool_models()
        self.assertFalse(result['data']['cmsd_available'])
        self.assertEqual(result['data']['observation'],'unconfirmed')
        self.assertNotIn('models',result['data'])
        self.assertEqual(result['data']['external_artifacts'],[])
    def test_store_catalog_marks_inventory_failure_unknown(self):
        with Fixture({'/v1/models':(503,ERROR),'/v1/status':(200,STATUS)}),patch.object(server,'_fetch',return_value=(200,b'[{"id":"target"}]')):
            result=server._tool_store_models()
        self.assertEqual(result['data']['local_observation'],'unconfirmed')
        self.assertEqual(result['data']['local_error'],ERROR['error'])
        self.assertEqual(result['data']['models'][0]['local_state'],'unknown')

    def test_partial_task_index_retains_store_query_and_never_claims_no_records(self):
        query = {'available':False,'error':{'code':'store_unreachable','message':'socket missing'}}
        for store_query in (None, query):
            snapshot = {'tasks':[]}
            if store_query is not None:
                snapshot['store_query'] = store_query
            with self.subTest(store_query=store_query), patch.object(server,'_http_get',return_value=(200,json.dumps(snapshot))), patch.object(server.megacube_tools.transfers,'list_tasks',return_value=[]):
                result = server._tool_task({})
            self.assertEqual(result['data']['cmsd_task_observation'],'available')
            self.assertEqual(result['data']['store_task_observation'],'unconfirmed')
            self.assertEqual(result['data']['store_query'],store_query)
            self.assertNotIn('没有任务记录',result['summary'])
            self.assertIn('商店任务尚未核实',result['summary'])

    def test_partial_task_index_retains_known_device_and_independent_jobs(self):
        query = {'available':False,'error':{'code':'store_unreachable','message':'socket missing'}}
        device = {'id':'device-a','kind':'switch','card':'target','status':'running','created_at':1}
        independent = {'id':'fetch-'+'a'*32,'kind':'fetch','status':'running','created_at':2}
        with patch.object(server,'_http_get',return_value=(200,json.dumps({'tasks':[device],'store_query':query}))), patch.object(server.megacube_tools.transfers,'list_tasks',return_value=[independent]):
            result = server._tool_task({})
        self.assertEqual({row['id'] for row in result['data']['tasks']},{device['id'],independent['id']})
        self.assertEqual(result['data']['store_query'],query)
        self.assertIn('商店任务尚未核实',result['summary'])

    def test_confirmed_empty_task_index_is_distinct_from_partial(self):
        with patch.object(server,'_http_get',return_value=(200,json.dumps({'tasks':[],'store_query':{'available':True}}))), patch.object(server.megacube_tools.transfers,'list_tasks',return_value=[]):
            result = server._tool_task({})
        self.assertEqual(result['data']['store_task_observation'],'available')
        self.assertIn('管理面没有任务记录',result['summary'])

    def test_partial_index_change_preserves_exact_task_lookup_failures(self):
        error={'error':{'code':'store_unreachable','message':'socket missing'}}
        with patch.object(server,'_http_get',return_value=(503,json.dumps(error))):
            result=server._tool_task({'task_id':'device-a','wait_seconds':0})
        self.assertEqual(result['data']['status'],'unknown')
        self.assertEqual(result['data']['observation_error_code'],'store_unreachable')
        self.assertNotIn('已成功',result['summary'])

if __name__=='__main__': unittest.main()
