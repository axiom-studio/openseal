"""Linux native smoke test. Run under xvfb-run and dbus-run-session with Vite running.
Uses only stdlib plus tauri-driver/WebKitWebDriver binaries; never a real model.
"""
import base64
import ctypes
import ctypes.util
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / 'desktop/src-tauri/target/debug/openseal-desktop'


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def close_native_window(window):
    # Send WM_DELETE_WINDOW directly: Xvfb has no window manager to handle
    # xdotool's _NET_CLOSE_WINDOW request on the root window.
    class Data(ctypes.Union):
        _fields_ = [('b', ctypes.c_char * 20), ('s', ctypes.c_short * 10), ('l', ctypes.c_long * 5)]
    class ClientMessage(ctypes.Structure):
        _fields_ = [('type', ctypes.c_int), ('serial', ctypes.c_ulong),
                    ('send_event', ctypes.c_int), ('display', ctypes.c_void_p),
                    ('window', ctypes.c_ulong), ('message_type', ctypes.c_ulong),
                    ('format', ctypes.c_int), ('data', Data)]
    class Event(ctypes.Union):
        _fields_ = [('client', ClientMessage), ('pad', ctypes.c_long * 24)]
    x11 = ctypes.CDLL(ctypes.util.find_library('X11'))
    x11.XOpenDisplay.argtypes = [ctypes.c_char_p]
    x11.XOpenDisplay.restype = ctypes.c_void_p
    x11.XInternAtom.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int]
    x11.XInternAtom.restype = ctypes.c_ulong
    x11.XSendEvent.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_long, ctypes.POINTER(Event)]
    x11.XFlush.argtypes = [ctypes.c_void_p]
    x11.XCloseDisplay.argtypes = [ctypes.c_void_p]
    display = x11.XOpenDisplay(None)
    assert display, 'Cannot open test X11 display'
    try:
        event = Event()
        event.client.type = 33  # ClientMessage
        event.client.display = display
        event.client.window = int(window)
        event.client.message_type = x11.XInternAtom(display, b'WM_PROTOCOLS', 0)
        event.client.format = 32
        event.client.data.l[0] = x11.XInternAtom(display, b'WM_DELETE_WINDOW', 0)
        assert x11.XSendEvent(display, int(window), 0, 0, ctypes.byref(event))
        x11.XFlush(display)
    finally:
        x11.XCloseDisplay(display)


@contextmanager
def model_fixture():
    held_turn = threading.Event()
    release_turn = threading.Event()
    guidance_started = threading.Event()
    release_guidance = threading.Event()
    intake_seen = set()
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass
        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            tool_name = payload['tools'][0]['function']['name']
            assert tool_name in ('submit_authoring_intent', 'submit_agent_turn', 'submit_channel_contribution')
            intent = {'schemaVersion': 'openseal.authoring-intent/v4', 'kind': 'agent', 'name': 'Native analyst', 'purpose': 'Analyze supplied evidence', 'agents': [{'key': 'native-analyst', 'name': 'Native analyst', 'purpose': 'Analyze supplied evidence', 'behavior': 'Analyze accurately and report uncertainty.'}]}
            if tool_name == 'submit_authoring_intent' and json.loads(payload['messages'][1]['content'])['prompt'].startswith('Create one team.'):
                intent = {'schemaVersion': 'openseal.authoring-intent/v4', 'kind': 'team', 'name': 'Native evidence team', 'purpose': 'Review supplied evidence together.',
                          'agents': [{'key': 'native-team-researcher', 'name': 'Native team researcher', 'purpose': 'Find evidence.', 'behavior': 'Cite evidence and report uncertainty.'}],
                          'team': {'key': 'native-evidence', 'name': 'Native evidence team', 'purpose': 'Review supplied evidence together.', 'operatingPrinciples': ['Keep uncertainty explicit.'], 'roles': [{'key': 'research', 'name': 'Research', 'purpose': 'Find evidence.', 'agentKeys': ['native-team-researcher'], 'canSpeakInChannels': True}]}}
            if tool_name == 'submit_authoring_intent' and intent.get('kind') == 'team':
                intent['agents'].append({'key': 'native-team-reviewer', 'name': 'Native team reviewer', 'purpose': 'Check evidence.', 'behavior': 'Review evidence independently.'})
                intent['team']['roles'].append({'key': 'review', 'name': 'Review', 'purpose': 'Check findings.', 'agentKeys': ['native-team-reviewer'], 'canSpeakInChannels': False})
            if tool_name == 'submit_agent_turn':
                task_input = json.loads(payload['messages'][1]['content'])
                guided = task_input['goal'] == 'Wait for native guidance'
                if guided:
                    if not task_input.get('pendingInterventions'):
                        guidance_started.set()
                        release_guidance.wait(30)
                    else:
                        assert task_input['pendingInterventions'][0]['instruction'] == 'Explain uncertainty before concluding.'
                if json.loads(payload['messages'][1]['content'])['goal'] == 'Wait for native cancellation':
                    held_turn.set()
                    release_turn.wait(30)
                intent = {'schemaVersion': 'openseal.hosted-turn-form/v1', 'nextRunStatus': 'completed', 'outputSummary': 'Analysis complete', 'runOutput': {'reply': 'Native task completed with uncertainty noted.', 'generatedFiles': [{'name': 'native-analysis.md', 'mediaType': 'text/markdown', 'text': '# Native analysis\nUncertainty remains explicit.\n'}]}}
            if tool_name == 'submit_agent_turn' and guided and task_input.get('pendingInterventions'):
                intent['runOutput']['reply'] = 'Native guidance considered: uncertainty explained.'
            if tool_name == 'submit_agent_turn':
                context_input = task_input.get('inputContext', {})
                checkpoint = task_input.get('continuationCheckpoint', {})
                if context_input.get('agentRequestInbox'):
                    request_id = context_input['agentRequestInbox']['requestId']
                    seen = request_id in intake_seen
                    intake_seen.add(request_id)
                    intent['runOutput'] = {'agentRequestDecision': {'decision': 'accept' if seen else 'request_clarification', 'message': 'Scope is clear.' if seen else 'Which reporting period should I use?'}}
                elif context_input.get('agentRequestCompletionReview'):
                    intent['runOutput'] = {'agentRequestCompletionReviewDecision': {'decision': 'approve', 'message': 'The result meets the stated scope.'}}
                elif task_input['goal'] == 'Native clarification review.' and not checkpoint.get('clarified'):
                    intent['runOutput'] = {}
                    if checkpoint.get('delegated') and not task_input.get('pendingInterventions'):
                        intent['nextRunStatus'] = 'paused'
                        intent['continuationCheckpoint'] = {'delegated': True}
                    else:
                        intent['nextRunStatus'] = 'running'
                        clarified = bool(task_input.get('pendingInterventions'))
                        if clarified:
                            assert 'July reporting period' in task_input['pendingInterventions'][0]['instruction']
                        intent['continuationCheckpoint'] = {'delegated': True, 'clarified': clarified}
                        budget = dict(task_input['budget']['minimumChild'])
                        if not clarified:
                            budget.update({'maxTurns': 6, 'maxAttempts': 8, 'maxTotalTokens': max(64000, budget.get('maxTotalTokens', 0))})
                        intent['proposedDelegation'] = {'stepId': 'native-check', 'assignedAgentId': task_input['eligibleAgents'][0]['id'], 'goal': 'Check the supplied reporting evidence.', 'checkpoint': {}, 'budget': budget}
                        if clarified:
                            intent['proposedDelegation']['clarification'] = 'Use the July reporting period.'
            if tool_name == 'submit_channel_contribution':
                intent = {'wantsToSpeak': True, 'content': 'Native team reply: verify the source and preserve uncertainty.', 'roleRelevant': True, 'hasNewInformation': True}
            response = json.dumps({'choices': [{'finish_reason': 'tool_calls', 'message': {'tool_calls': [{'id': 'native-fixture', 'type': 'function', 'function': {'name': tool_name, 'arguments': json.dumps(intent)}}]}}]}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(response)))
            self.end_headers()
            self.wfile.write(response)
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        yield f'http://127.0.0.1:{server.server_port}/v1', held_turn, release_turn, guidance_started, release_guidance
    finally:
        release_turn.set()
        release_guidance.set()
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)


def main():
    driver_path = shutil.which('tauri-driver') or str(Path.home() / '.cargo/bin/tauri-driver')
    port, native_port = free_port(), free_port()
    with model_fixture() as fixture, tempfile.TemporaryDirectory(prefix='openseal-native-smoke-') as directory:
        provider_url, held_turn, release_turn, guidance_started, release_guidance = fixture
        env = {**os.environ, 'XDG_DATA_HOME': directory, 'XDG_CONFIG_HOME': directory + '/config', 'WEBKIT_DISABLE_DMABUF_RENDERER': '1', 'GDK_BACKEND': 'x11', 'GDK_SCALE': '1', 'GDK_DPI_SCALE': '1', 'GTK_USE_PORTAL': '0'}
        env.pop('WAYLAND_DISPLAY', None)
        driver = subprocess.Popen([driver_path, '--port', str(port), '--native-port', str(native_port)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        session = ''
        def call(path, body=None, method=None):
            data = None if body is None else json.dumps(body).encode()
            request = urllib.request.Request(f'http://127.0.0.1:{port}' + path, data=data, method=method, headers={'Content-Type': 'application/json'})
            return json.load(urllib.request.urlopen(request, timeout=45))['value']
        def execute(script):
            return call('/session/' + session + '/execute/sync', {'script': script, 'args': []})
        def until(script):
            deadline = time.monotonic() + 35
            while time.monotonic() < deadline:
                if execute(script):
                    return
                time.sleep(.2)
            raise AssertionError('Native UI did not reach expected state')
        def fill(selector, text):
            element = call('/session/' + session + '/element', {'using': 'css selector', 'value': selector})['element-6066-11e4-a52e-4f735466cecf']
            prefix = '/session/' + session + '/element/' + element
            call(prefix + '/clear', {})
            call(prefix + '/value', {'text': text})
        def close_and_wait():
            # Deliver an actual OS close event and await owned-daemon shutdown.
            windows = subprocess.check_output(['xdotool', 'search', '--name', '^OpenSeal$'], env=env, text=True).splitlines()
            assert windows, 'Native X11 window not found'
            for window in windows:
                close_native_window(window)
            deadline = time.monotonic() + 8
            leaked = []
            while time.monotonic() < deadline:
                leaked = []
                for proc in Path('/proc').iterdir():
                    if not proc.name.isdigit():
                        continue
                    try:
                        cmdline = (proc / 'cmdline').read_bytes()
                        if b'daemon\x00--config' in cmdline and directory.encode() in cmdline:
                            leaked.append(proc.name)
                    except OSError:
                        pass
                if not leaked:
                    break
                time.sleep(.1)
            assert not leaked, f'Owned daemon still running: {leaked}'
        try:
            for _ in range(100):
                try:
                    call('/status')
                    break
                except (OSError, urllib.error.URLError):
                    if driver.poll() is not None:
                        raise RuntimeError('Native driver stopped')
                    time.sleep(.1)
            result = call('/session', {'capabilities': {'alwaysMatch': {'tauri:options': {'application': str(BINARY)}}}})
            session = result['sessionId']
            until("return !!window.__TAURI_INTERNALS__ && document.body.innerText.includes('Connected locally')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Settings').click(); return true")
            until("return !!document.querySelector('input[aria-label=\"Provider URL\"]:not(:disabled)')")
            fill('input[aria-label="Provider URL"]', provider_url)
            fill('input[aria-label="Model"]', 'native-test-model')
            fill('input[aria-label="API key"]', 'synthetic-native-key')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Save and connect').click(); return true")
            until("return document.body.innerText.includes('Provider settings saved. Workspace reconnected.')")
            assert execute("return document.querySelector('input[aria-label=\"API key\"]').value") == ''
            settings = call('/session/' + session + '/execute/async', {'script': "const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('load_provider_settings').then(done, error=>done({error}));", 'args': []})
            assert settings == {'baseUrl': provider_url, 'model': 'native-test-model', 'hasApiKey': True}, settings
            context = Path(directory) / 'studio.axiom.openseal/context.yaml'
            assert context.exists()
            assert 'synthetic-native-key' not in context.read_text()
            secrets = list((context.parent / '.secrets').glob('*.key'))
            assert len(secrets) == 1
            assert secrets[0].stat().st_mode & 0o777 == 0o600
            assert secrets[0].read_text() == 'synthetic-native-key'
            call('/session/' + session + '/window/rect', {'width': 1280, 'height': 1300})
            execute('window.scrollTo(0, 0); return true')
            output = ROOT / '.impeccable/review/provider-native-linux.png'
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            subprocess.run(['go', 'run', 'tests/seed_team.go', '-db', str(context.parent / 'data/openseal.db')], cwd=ROOT / 'desktop', check=True, timeout=30)
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Teams')).click(); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Synthetic review team')")
            assert execute("return [...document.querySelectorAll('.record-main strong')].filter(el=>el.textContent==='Synthetic review team').length") == 1
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Synthetic review team').closest('button').click(); return true")
            until("return document.body.innerText.includes('Report uncertainty clearly.')")
            assert execute("return document.querySelector('.team-details').innerText.includes('No member assigned.')")
            (output.parent / 'teams-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Home')).click(); return true")
            until("return !!document.querySelector('textarea#prompt')")
            fill('textarea#prompt', 'Create an inactive Native analyst agent.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Create proposal').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Check installation')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Check installation').click(); return true")
            until("return !!document.querySelector('.installation-consent input')")
            assert execute("return [...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Approve installation').disabled")
            execute("document.querySelector('.installation-consent input').click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Approve installation').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Install without activating')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Install without activating').click(); return true")
            until("return [...document.querySelectorAll('h3')].some(h=>h.textContent.trim()==='Installed')")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Review activation')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Review activation').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Check activation')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Check activation').click(); return true")
            until("return !!document.querySelector('.installation-consent input')")
            assert execute("return [...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Approve activation').disabled")
            execute("document.querySelector('.installation-consent input').click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Approve activation').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Activate resources')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Activate resources').click(); return true")
            until("return [...document.querySelectorAll('h3')].some(h=>h.textContent.trim()==='Activated')")
            execute('document.activeElement?.blur(); window.scrollTo(0, 0); return true')
            (output.parent / 'activation-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='View agents').click(); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Native analyst')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Native analyst').closest('button').click(); return true")
            until("return !!document.querySelector('aside button.primary')")
            execute("document.querySelector('aside button.primary').click(); return true")
            until("return !!document.querySelector('textarea#prompt')")
            fill('textarea#prompt', 'Analyze the supplied evidence and note uncertainty.')
            execute("document.querySelector('form button[type=submit]').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Native task completed with uncertainty noted.')")
            execute('document.activeElement?.blur(); window.scrollTo(0, 0); return true')
            (output.parent / 'work-result-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Home')).click(); return true")
            until("return !!document.querySelector('textarea#prompt')")
            fill('textarea#prompt', 'Wait for native guidance')
            execute("document.querySelector('form button[type=submit]').click(); return true")
            assert guidance_started.wait(10), 'Native guidance task did not reach provider'
            until("return [...document.querySelectorAll('.work-guidance button')].some(b=>b.textContent==='Add guidance')")
            execute("[...document.querySelectorAll('.work-guidance button')].find(b=>b.textContent==='Add guidance').click(); return true")
            fill('.work-guidance textarea', 'Explain uncertainty before concluding.')
            execute("document.querySelector('.work-guidance button[type=submit]').click(); return true")
            until("return document.querySelector('.work-guidance')?.innerText.includes('Guidance saved in this task')")
            release_guidance.set()
            until("return document.querySelector('.inspector')?.innerText.includes('Native guidance considered: uncertainty explained.')")
            execute("document.querySelector('.work-guidance details summary').click(); document.querySelector('.work-guidance').scrollIntoView(); return true")
            (output.parent / 'work-guidance-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Home')).click(); return true")
            until("return !!document.querySelector('textarea#prompt')")
            fill('textarea#prompt', 'Wait for native cancellation')
            execute("document.querySelector('form button[type=submit]').click(); return true")
            assert held_turn.wait(10), 'Native task did not reach the model'
            until("return document.querySelector('.inspector')?.innerText.includes('Running')")
            execute("[...document.querySelectorAll('.inspector button')].find(b=>b.textContent.trim()==='Cancel work').click(); return true")
            until("return document.activeElement?.textContent.trim()==='Confirm cancellation'")
            execute("[...document.querySelectorAll('.inspector button')].find(b=>b.textContent.trim()==='Confirm cancellation').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Work canceled. Saved results remain available.')")
            release_turn.set()
            execute('document.activeElement?.blur(); window.scrollTo(0, 0); return true')
            (output.parent / 'work-cancel-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('.inspector button')].find(b=>b.textContent.trim()==='Review a new attempt').click(); return true")
            until("return document.querySelector('textarea#prompt')?.value==='Wait for native cancellation' && document.activeElement?.id==='prompt'")


            close_and_wait()
            call('/session/' + session, method='DELETE')
            session = ''
            result = call('/session', {'capabilities': {'alwaysMatch': {'tauri:options': {'application': str(BINARY)}}}})
            session = result['sessionId']
            until("return document.body.innerText.includes('Connected locally')")
            until("return document.querySelector('textarea#prompt')?.value==='Wait for native cancellation'")
            until("return document.querySelector('.composer-tabs button[aria-pressed=true]')?.textContent.trim()==='Start work'")
            until("return document.querySelector('.agent-picker select')?.selectedOptions[0]?.textContent==='Native analyst'")
            assert execute("return document.querySelector('form button[type=submit]').disabled") is False
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Settings').click(); return true")
            until("return document.querySelector('input[aria-label=\"Model\"]')?.value === 'native-test-model'")
            assert execute("return document.querySelector('input[aria-label=\"Provider URL\"]').value") == provider_url
            assert execute("return document.querySelector('input[aria-label=\"API key\"]').value") == ''
            assert execute("return document.body.innerText.includes('Saved securely')")
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Agents')).click(); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Native analyst')")
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Work')).click(); return true")
            fill('input[type="search"]', 'no matching native task')
            until("return document.body.innerText.includes('No work matches this view')")
            fill('input[type="search"]', 'supplied evidence')
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Analyze the supplied evidence and note uncertainty.')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Analyze the supplied evidence and note uncertainty.').closest('button').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Native task completed with uncertainty noted.')")
            execute("document.querySelector('.task-artifacts summary').click(); return true")
            until("return [...document.querySelectorAll('.artifact-list h4')].some(el=>el.textContent==='native-analysis.md')")
            execute("document.querySelector('.artifact-list button').click(); return true")
            until("return document.querySelector('.artifact-list pre')?.textContent==='# Native analysis\\nUncertainty remains explicit.\\n'")
            assert execute("return document.activeElement===document.querySelector('.artifact-list pre')")
            subprocess.run(['go', 'run', str(ROOT / 'desktop/tests/seed_approval.go'), '-db', str(context.parent / 'data/openseal.db')], cwd=ROOT, stdout=subprocess.DEVNULL, check=True, timeout=30)
            execute("document.querySelector('.inspector button[title]').click(); return true")
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Reviews')).click(); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Review a synthetic action')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Review a synthetic action').closest('button').click(); return true")
            until("return document.querySelector('.action-approval')?.innerText.includes('Review a synthetic action')")
            execute("document.querySelector('.approval-review-check input').click(); return true")
            execute("[...document.querySelectorAll('.action-approval button')].find(b=>b.textContent==='Approve action').click(); return true")
            execute("[...document.querySelectorAll('.action-approval button')].find(b=>b.textContent==='Confirm: approve action').click(); return true")
            until("return document.querySelector('.action-approval')?.innerText.includes('Action approved. Execution remains')")
            recorded = call('/session/' + session + '/execute/async', {'script': "const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('api_request', {method:'GET', path:'/api/v1/action-approvals/ui-approval?scopeKind=local&scopeId=default', body:null, idempotencyKey:null}).then(done, error=>done({error}));", 'args': []})
            assert recorded['body']['status'] == 'approved', recorded
            assert recorded['body']['decisionBy'] == {'type': 'user', 'id': 'local-operator'}, recorded
            execute("document.querySelector('.inspector button[title]').click(); return true")
            execute("const el=document.querySelector('.filter-select select'); el.value='approved'; el.dispatchEvent(new Event('change',{bubbles:true})); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Review a synthetic action')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Review a synthetic action').closest('button').click(); return true")
            until("return document.querySelector('.action-approval')?.innerText.includes('Reviewed by local-operator.')")
            subprocess.run(['go', 'run', str(ROOT / 'desktop/tests/seed_artifact.go'), '-db', str(context.parent / 'data/openseal.db')], cwd=ROOT, stdout=subprocess.DEVNULL, check=True, timeout=30)
            execute("document.querySelector('.inspector button[title]').click(); return true")
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Work')).click(); return true")
            fill('input[type="search"]', 'Inspect synthetic task artifacts')
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Inspect synthetic task artifacts')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Inspect synthetic task artifacts').closest('button').click(); return true")
            execute("document.querySelector('.task-artifacts summary').click(); return true")
            until("return document.querySelectorAll('.artifact-list li').length===3")
            execute("[...document.querySelectorAll('.artifact-list li')].find(el=>el.innerText.includes('report.txt') && el.innerText.includes('Version 2')).querySelector('button').click(); return true")
            until("return document.querySelector('.artifact-list pre')?.textContent==='Verified task artifact.\\nUncertainty remains explicit.\\n'")
            execute("document.querySelector('.artifact-list').scrollIntoView(); return true")
            (output.parent / 'artifacts-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))

            def save_binary():
                execute("[...document.querySelectorAll('.artifact-list li')].find(el=>el.innerText.includes('evidence.bin')).querySelector('button').click(); return true")
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    found = subprocess.run(['xdotool', 'search', '--onlyvisible', '--name', '^Save artifact$'], capture_output=True, text=True)
                    if found.returncode == 0:
                        window = found.stdout.strip().splitlines()[-1]
                        subprocess.run(['xdotool', 'windowfocus', '--sync', window], check=True)
                        return window
                    time.sleep(.1)
                raise AssertionError('Native Save artifact dialog did not appear')

            save_binary()
            subprocess.run(['xdotool', 'key', 'Escape'], check=True)
            until("return document.querySelector('.task-artifacts')?.innerText.includes('Save canceled.')")
            destination = Path(directory) / 'renamed-evidence.bin'
            save_binary()
            subprocess.run(['xdotool', 'key', 'ctrl+a'], check=True)
            subprocess.run(['xdotool', 'type', '--clearmodifiers', str(destination)], check=True)
            subprocess.run(['xdotool', 'key', 'Return'], check=True)
            until("return document.querySelector('.task-artifacts')?.innerText.includes('Artifact saved.')")
            assert destination.read_bytes() == bytes([0,255,128,13,10,1,254])
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Teams')).click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Create team').click(); return true")
            until("return !!document.querySelector('textarea#prompt')")
            fill('textarea#prompt', 'Review supplied evidence with a research role.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Create proposal').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Check installation')")
            assert execute("return document.querySelector('.proposal-team').innerText.includes('Native team researcher')")
            assert execute("return document.querySelector('.proposal-team').innerText.includes('Maximum team risk:')")
            (output.parent / 'team-proposal-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Check installation').click(); return true")
            until("return !!document.querySelector('.installation-consent input')")
            execute("document.querySelector('.installation-consent input').click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Approve installation').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Install and activate')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Install and activate').click(); return true")
            until("return [...document.querySelectorAll('h3')].some(h=>h.textContent.trim()==='Installed')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='View teams').click(); return true")
            until("return [...document.querySelectorAll('.record-main strong')].some(el=>el.textContent==='Native evidence team')")
            execute("[...document.querySelectorAll('.record-main strong')].find(el=>el.textContent==='Native evidence team').closest('button').click(); return true")
            until("return document.querySelector('.team-details')?.innerText.includes('2 members')")
            assert execute("return document.querySelector('.team-details').innerText.includes('Native team researcher')")
            (output.parent / 'team-installed-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Pause team').click(); return true")
            until("return !!document.querySelector('.team-controls textarea')")
            fill('.team-controls textarea', 'Review native team readiness.')
            (output.parent / 'team-controls-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Confirm pause').click(); return true")
            until("return document.querySelector('.team-controls')?.innerText.includes('Team paused.')")
            execute("document.querySelector('.team-history summary').click(); return true")
            until("return document.querySelector('.team-history')?.innerText.includes('Review native team readiness.')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Resume team').click(); return true")
            fill('.team-controls textarea', 'Ready to coordinate again.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Confirm resume').click(); return true")
            until("return document.querySelector('.team-controls')?.innerText.includes('Team resumed.')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Edit roster').click(); return true")
            until("return !!document.querySelector('.team-roster textarea')")
            fill('.team-roster input:not([type="checkbox"])', 'Native research lead')
            fill('.team-roster textarea', 'Clarify native team responsibility.')
            execute("document.querySelector('.team-roster input[type=checkbox]').click(); document.querySelector('button[aria-label=\"Dismiss notification\"]')?.click(); document.activeElement?.blur(); document.querySelector('.team-roster').scrollIntoView({block:'end'}); return true")
            (output.parent / 'team-roster-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Save roster').click(); return true")
            until("return document.querySelector('.team-roster')?.innerText.includes('Team roster saved.')")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='View Native research lead')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Channels').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='New channel')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='New channel').click(); return true")
            fill('.channel-composer input', 'Native evidence notes')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Create channel').click(); return true")
            until("return document.querySelector('.channel-thread h3')?.innerText==='Native evidence notes'")
            fill('.channel-thread textarea', 'Keep the July reporting period explicit.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Post message').click(); return true")
            until("return document.querySelector('.channel-messages')?.innerText.includes('Keep the July reporting period explicit.')")
            until("return document.activeElement?.innerText==='Message posted.'")
            execute("document.activeElement?.blur(); document.querySelector('.channel-thread').scrollIntoView({block:'start'}); return true")
            (output.parent / 'team-channels-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Reply to message 1').click(); return true")
            fill('.channel-thread textarea', 'Confirmed: use July evidence.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Post message').click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Original message')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Original message').click(); return true")
            until("return document.activeElement?.innerText==='Original message · Message 1'")
            assert execute("return document.querySelector('.channel-context-body')?.innerText.includes('Keep the July reporting period explicit.')")
            execute("document.querySelector('.channel-context').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-context-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Close message').click(); return true")
            until("return document.activeElement?.innerText==='Original message'")
            linked = call('/session/' + session + '/execute/async', {'script': """
                const done=arguments[arguments.length-1];
                const request=(method,path,body=null,key=null)=>window.__TAURI_INTERNALS__.invoke('api_request',{method,path,body,idempotencyKey:key});
                (async()=>{
                    const list=await request('GET','/api/v1/conversations?scopeKind=local&scopeId=default&limit=100');
                    const channel=list.body.find(c=>c.title==='Native evidence notes');
                    if(!channel) throw new Error('Native channel missing');
                    return request('POST','/api/v1/conversations/'+encodeURIComponent(channel.id)+'/messages',{
                        scope:{kind:'local',id:'default'},expectedRevision:channel.revision,
                        sender:{type:'user',id:'local-operator'},intent:'update',audience:{kind:'channel'},
                        content:'Review this exact report version and its producing work.',
                        references:[{kind:'artifact',id:'ui:résumé',version:1},{kind:'run',id:'ui-artifact-run'},{kind:'approval',id:'ui-approval'}]
                    },'native-reference-message');
                })().then(done,error=>done({error:String(error)}));
            """, 'args': []})
            assert linked.get('status') == 201, linked
            until("return [...document.querySelectorAll('.channel-references button')].some(b=>b.textContent.includes('Artifact · ui:résumé'))")
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent.includes('Artifact · ui:résumé')).click(); return true")
            until("return !!document.querySelector('.channel-references .artifact-list button')")
            execute("document.querySelector('.channel-references .artifact-list button').click(); return true")
            until("return document.querySelector('.channel-references pre')?.textContent==='First saved report.\\n'")
            assert execute("return document.activeElement===document.querySelector('.channel-references pre')")
            execute("document.querySelector('.channel-references .channel-context-body').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-references-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent.trim()==='Work · ui-artifact-run').click(); return true")
            until("return [...document.querySelectorAll('.channel-references button')].some(b=>b.textContent==='Open work inspector')")
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent==='Open work inspector').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Inspect synthetic task artifacts')")
            execute("document.querySelector('.inspector button[title]').click(); return true")
            until("return document.activeElement?.innerText==='Open work inspector'")
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent==='Close reference').click(); return true")
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent.trim()==='Approval · ui-approval').click(); return true")
            until("return [...document.querySelectorAll('.channel-references button')].some(b=>b.textContent==='Open approval review')")
            assert execute("return document.querySelector('.channel-references .channel-context-body')?.innerText.includes('approved')")
            execute("document.querySelector('.channel-references .channel-context-body').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-decision-links-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent==='Open approval review').click(); return true")
            until("return document.querySelector('.action-approval')?.innerText.includes('Reviewed by local-operator.')")
            assert execute("return ![...document.querySelectorAll('.action-approval button')].some(b=>b.textContent==='Approve action')")
            execute("document.querySelector('.inspector button[title]').click(); return true")
            until("return document.activeElement?.innerText==='Open approval review'")
            execute("[...document.querySelectorAll('.channel-references button')].find(b=>b.textContent==='Close reference').click(); return true")
            until("return document.querySelector('.channel-read-state')?.innerText.includes('3 unread messages')")
            until("return [...document.querySelectorAll('.channel-picker button')].some(b=>b.textContent==='Native evidence notes · 3 unread')")
            execute("document.querySelector('.channel-picker').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-unread-list-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))

            for _ in range(2):
                execute("[...document.querySelectorAll('.channel-read-state button')].find(b=>b.textContent==='Start at unread messages').click(); return true")
                until("return document.querySelector('.channel-thread')?.innerText.includes('Messages 1–3 · From your unread messages.') && !!document.querySelector('.channel-messages li')")
            execute("document.querySelector('.channel-read-state').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-read-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('.channel-read-state button')].find(b=>b.textContent==='Mark read through message 3').click(); return true")
            until("return document.querySelector('.channel-read-state')?.innerText.includes('Marked read through message 3.')")
            until("return [...document.querySelectorAll('.channel-picker button')].some(b=>b.textContent==='Native evidence notes')")
            assert execute("return document.activeElement===document.querySelector('.channel-read-state [role=status]')")
            execute("[...document.querySelectorAll('.channel-read-state button')].find(b=>b.textContent==='Refresh read position').click(); return true")
            until("return document.querySelector('.channel-read-state')?.innerText.includes('Saved read position refreshed.')")
            assert execute("return document.querySelector('.channel-read-state')?.innerText.includes('You’re caught up')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Channel settings').click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Enable team replies').click(); return true")
            until("return document.querySelector('.channel-settings')?.innerText.includes('Team replies are on.')")
            fill('.channel-thread textarea', 'Review this new evidence with the team.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Post message').click(); return true")
            until("return document.querySelector('.channel-thread')?.innerText.includes('Native team reply: verify the source and preserve uncertainty.')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Disable team replies').click(); return true")
            until("return document.querySelector('.channel-settings')?.innerText.includes('Team replies are off.')")
            execute("document.querySelector('.channel-settings .channel-availability').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-participation-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            fill('.channel-settings input', 'Reviewed native evidence')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Save channel name').click(); return true")
            until("return document.querySelector('.channel-settings')?.innerText.includes('Channel renamed.')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Archive channel').click(); return true")
            execute("document.activeElement?.blur(); document.querySelector('.channel-thread').scrollIntoView({block:'start'}); return true")
            (output.parent / 'channel-settings-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Confirm archive').click(); return true")
            until("return document.querySelector('.channel-settings')?.innerText.includes('Channel archived.')")
            assert execute("return [...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Post message').disabled")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Restore channel').click(); return true")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Confirm restore').click(); return true")
            until("return document.querySelector('.channel-settings')?.innerText.includes('Channel restored.')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Use as team work').click(); return true")
            until("return document.activeElement?.innerText==='Use this message as team work'")
            execute("document.querySelector('.channel-work-handoff').scrollIntoView({block:'center'}); return true")
            (output.parent / 'channel-work-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Use message in draft').click(); return true")
            until("return document.activeElement===document.querySelector('.team-work textarea')")
            assert execute("return document.querySelector('.team-work textarea').value==='Keep the July reporting period explicit.'")
            execute("const el=document.querySelector('.team-work select'); el.value=el.options[1].value; el.dispatchEvent(new Event('change',{bubbles:true})); return true")
            execute("document.activeElement?.blur(); document.querySelector('.team-work form').scrollIntoView({block:'end'}); return true")
            (output.parent / 'team-work-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Start team work').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Native task completed with uncertainty noted.')")
            execute("document.querySelector('.inspector button[title]').click(); return true")
            execute("const el=document.querySelector('.team-work select'); el.value=el.options[1].value; el.dispatchEvent(new Event('change',{bubbles:true})); return true")
            fill('.team-work textarea', 'Native clarification review.')
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Start team work').click(); return true")
            until("return [...document.querySelectorAll('.inspector button')].some(b=>b.textContent.trim()==='Resume')")
            execute("[...document.querySelectorAll('nav button')].find(b=>b.textContent.trim().startsWith('Work'))?.click(); return true")
            until("return [...document.querySelectorAll('button')].some(b=>b.textContent.trim()==='Requests')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Requests').click(); return true")
            until("return document.querySelector('[aria-label=\"Workspace requests\"] .record-row')?.innerText.includes('Which reporting period')")
            (output.parent / 'request-inbox-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("document.querySelector('[aria-label=\"Workspace requests\"] .record-row').click(); return true")
            until("return document.querySelector('.work-collaboration')?.innerText.includes('Which reporting period should I use?')")
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Guide the lead').click(); return true")
            fill('.work-guidance textarea', 'Use the July reporting period for this request.')
            execute("document.activeElement?.blur(); document.querySelector('.work-guidance').scrollIntoView({block:'end'}); return true")
            (output.parent / 'work-collaboration-native-linux.png').write_bytes(base64.b64decode(call('/session/' + session + '/screenshot')))
            execute("[...document.querySelectorAll('button')].find(b=>b.textContent.trim()==='Save guidance').click(); return true")
            until("return document.querySelector('.work-guidance')?.innerText.includes('Guidance saved in this task')")
            execute("[...document.querySelectorAll('.inspector button')].find(b=>b.textContent.trim()==='Resume').click(); return true")
            until("return document.querySelector('.inspector')?.innerText.includes('Native task completed with uncertainty noted.')")
            close_and_wait()
            print('Native smoke passed: collaboration question, human guidance, resumed delegation and result; team-owned work start and result; reviewed roster edit; team proposal, approval, installation, member inspection, pause/resume and audited reasons; scoped Teams catalog, roles and native rendering; artifact text preview, native Save As and cancellation, binary checksum verified export; provider form, private key storage, inactive installation, reviewed activation, task execution, result, cancellation and retry draft, scoped action approval, owned-daemon shutdown, and settings plus installed agent and completed task restored after reopening.')
        finally:
            if session:
                try:
                    call('/session/' + session, method='DELETE')
                except (OSError, urllib.error.URLError):
                    pass
            driver.terminate()
            try:
                driver.wait(timeout=5)
            except subprocess.TimeoutExpired:
                driver.kill()
                driver.wait()


if __name__ == '__main__':
    main()
