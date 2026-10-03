#!/usr/bin/env python3
"""Generate emulator-backed vectors for the Farm dialog's evolution.

Each case runs, in ONE emulated plugin process (the global grammar lives
for the whole process), one or two generations of the dialog's Breed
command (tools/emu.py Plugin.breed): load the session, grow every cell,
rate each active cell by its slider, regrow the active cells, save. A
case may instead start from the dialog's New Session. The saved .gds
bytes of each generation are stored; TestEvolveVectors replays them with
farm.LoadFarm / Farm.Breed / farm.NewSession.

    python3 tools/gen_evolve.py   # writes internal/farm/testdata/evolve.json.gz
"""
import base64
import gzip
import json
import os
import random
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from emu import Plugin  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))



def require_plugin(path):
    """The original ffarm.8bf is not distributed with ffarmgo; put a copy in
    real_plugin/ (with ffarm1.png for the reference check) to use this tool."""
    if not os.path.exists(path):
        sys.exit('missing %s: copy the original Filter Farm plugin there '
                 '(it is not part of the ffarmgo release)' % path)
    return path

def with_active(gds, n):
    return gds[:2] + struct.pack('<i', n) + gds[6:]


def main():
    plugin = require_plugin(os.path.join(ROOT, 'real_plugin', 'ffarm.8bf'))
    real = open(os.path.join(ROOT, 'internal', 'gdo', 'testdata', 'render.gds'), 'rb').read()
    rng = random.Random(3)
    cases = [
        dict(name='real_active1', input=real, w=64, h=48, image=True, gens=[[10], [90]]),
        dict(name='real_active3', input=with_active(real, 3), w=64, h=48, image=True,
             gens=[[0, 50, 100], [70, 30, 50]]),
        dict(name='real_active9', input=with_active(real, 9), w=40, h=30, image=True,
             gens=[[rng.randint(0, 100) for _ in range(9)] for _ in range(2)]),
        dict(name='real_scalar_root', input=with_active(real, 3), w=64, h=48, image=False,
             gens=[[100, 0, 20], [50, 85, 5]]),
        dict(name='new_session', new_session=0x1234, w=48, h=36, image=True,
             gens=[[100, 10, 60], [0, 95, 40]]),
    ]
    out = []
    for c in cases:
        p = Plugin(plugin)
        vec = dict(name=c['name'], w=c['w'], h=c['h'], image=c['image'], gens=[])
        if 'new_session' in c:
            gds = p.new_session(c['w'], c['h'], c['new_session'], c['image'])
            vec['new_session'] = c['new_session']
            vec['new_out'] = base64.b64encode(gds).decode()
        else:
            gds = c['input']
            vec['input'] = base64.b64encode(gds).decode()
        for sliders in c['gens']:
            gds = p.breed(gds, c['w'], c['h'], sliders, c['image'])
            vec['gens'].append(dict(sliders=sliders, out=base64.b64encode(gds).decode()))
        out.append(vec)
        print(c['name'], 'done', flush=True)
    path = os.path.join(ROOT, 'internal', 'farm', 'testdata', 'evolve.json.gz')
    with gzip.open(path, 'wt') as fh:
        json.dump(out, fh)
    print('wrote', path, os.path.getsize(path), 'bytes')


if __name__ == '__main__':
    main()
