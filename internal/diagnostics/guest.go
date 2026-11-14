package diagnostics

import (
	"encoding/json"
	"strings"
)

// GuestRunner uses the same redaction expressions as the host, before journald
// receives service output. stdin and systemd notify sockets remain inherited.
func GuestRunner() string {
	rules := make([][2]string, 0, len(patterns))
	for _, p := range patterns {
		rules = append(rules, [2]string{p.re.String(), strings.ReplaceAll(p.replacement, "${1}", `\g<1>`)})
	}
	b, _ := json.Marshal(rules)
	return "#!/usr/bin/python3\n" + `import json, os, re, select, signal, subprocess, sys
rules = [(re.compile(pattern), replacement) for pattern, replacement in json.loads(` + string(mustJSON(string(b))) + `)]
child = subprocess.Popen(sys.argv[1:], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
stopped_signal = 0
def stop(sig, _frame):
    global stopped_signal
    stopped_signal = sig
    try:
        child.send_signal(sig)
    except ProcessLookupError:
        pass
# Python runs handlers between bytecodes. A signal that lands after the last
# check but before a blocking read only runs once the child prints again, so
# an idle child kept running until systemd's stop timeout. The wakeup pipe
# makes select return for every signal, wherever it lands.
wakeup, notify = os.pipe()
os.set_blocking(notify, False)
signal.set_wakeup_fd(notify)
for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(sig, stop)
private_key = False
def emit(data):
    global private_key
    if len(data) > 24576:
        print('[oversized output line omitted]', flush=True)
        return
    line = data.decode('utf-8', 'replace')
    if '-----BEGIN ' in line:
        private_key = True
    if private_key:
        if '-----END ' in line:
            private_key = False
        return
    for pattern, replacement in rules:
        line = pattern.sub(lambda match: match.expand(replacement), line)
    print(line.rstrip(), flush=True)
output = child.stdout.fileno()
pending = b''
skipping = False
while True:
    readable = select.select([output, wakeup], [], [])[0]
    if wakeup in readable:
        os.read(wakeup, 512)
    if output not in readable:
        continue
    data = os.read(output, 65536)
    if not data:
        break
    *lines, pending = (pending + data).split(b'\n')
    for line in lines:
        if skipping:
            skipping = False
        else:
            emit(line + b'\n')
    if len(pending) > 24576:
        if not skipping:
            emit(pending)
        pending, skipping = b'', True
if pending and not skipping:
    emit(pending)
code = child.wait()
if stopped_signal and code in (-stopped_signal, 128 + stopped_signal):
    code = 0
sys.exit(code if code >= 0 else 128-code)
`
}
func mustJSON(v string) []byte { b, _ := json.Marshal(v); return b }
