#!/usr/bin/env python3
"""Opt-in real CPU inference for isolated single-node acceptance; no GPU access."""
import json
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
from transformers import AutoModelForCausalLM, AutoTokenizer

# Python running as container PID 1 needs an explicit termination handler.
signal.signal(signal.SIGTERM, lambda *_: exit(0))

torch.set_num_threads(2)
tokenizer = AutoTokenizer.from_pretrained('/qa/model', local_files_only=True)
model = AutoModelForCausalLM.from_pretrained('/qa/model', local_files_only=True,
                                          torch_dtype=torch.float32).eval().to('cpu')
MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'
gate = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def reply(self, value, status=200):
        payload = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path == '/v1/models':
            return self.reply({'object': 'list', 'data': [{'id': MODEL_ID, 'object': 'model', 'max_model_len': 2048}]})
        self.reply({'error': {'code': 'not_found'}}, 404)

    def do_POST(self):
        if self.path != '/v1/chat/completions':
            return self.reply({'error': {'code': 'not_found'}}, 404)
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if body.get('model') != MODEL_ID:
            return self.reply({'error': {'code': 'model_not_found'}}, 404)
        prompt = tokenizer.apply_chat_template(body['messages'], tokenize=False, add_generation_prompt=True)
        inputs = tokenizer(prompt, return_tensors='pt', truncation=True, max_length=512)
        with gate, torch.inference_mode():
            output = model.generate(**inputs, do_sample=False, max_new_tokens=min(64, body.get('max_tokens', 32)),
                                    pad_token_id=tokenizer.eos_token_id)
        text = tokenizer.decode(output[0][inputs['input_ids'].shape[1]:], skip_special_tokens=True)
        response = {'id': 'cpu-qa-'+str(time.time_ns()), 'object': 'chat.completion', 'model': MODEL_ID,
                    'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': text}, 'finish_reason': 'stop'}]}
        if body.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Connection', 'close')
            self.end_headers()
            chunk = {'id': response['id'], 'object': 'chat.completion.chunk', 'model': MODEL_ID,
                     'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': text}, 'finish_reason': None}]}
            self.wfile.write(('data: '+json.dumps(chunk)+'\n\ndata: [DONE]\n\n').encode())
            self.close_connection = True
        else:
            self.reply(response)


ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
