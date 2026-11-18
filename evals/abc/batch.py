"""Run a list of (scenario, arm, n) sequentially; stop on budget or error. Usage: batch.py sid:arm:n ..."""
import sys, traceback
sys.path.insert(0, '.')
import driver as d
for spec in sys.argv[1:]:
    parts = spec.split(':')
    sid, arm, n = parts[:3]
    agent = d.PROD if len(parts) > 3 and parts[3] == 'product' else d.ENG
    try:
        d.run(sid, arm, int(n), agent)
    except SystemExit as e:
        print('STOP', spec, e, flush=True); break
    except Exception:
        print('ERROR', spec, traceback.format_exc()[-1500:], flush=True); break
    sys.stdout.flush()
print('BATCH DONE', d.spend(), flush=True)
