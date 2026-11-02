#!/usr/bin/env python3
"""Exercise the publication guard's actual workflow body without registry access."""

import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[2]
workflow = (root / '.github/workflows/build-gateway-0.2.19-image.yml').read_text()
marker = '      - name: Refuse to overwrite a candidate tag\n'
assert workflow.count(marker) == 1
block = workflow.split(marker, 1)[1].split('\n      - name:', 1)[0]
body = block.split('        run: |\n', 1)[1]
script = '\n'.join(line[10:] for line in body.splitlines() if line.startswith('          '))
assert 'docker buildx imagetools inspect' in script
image = 'ghcr.io/xz-dev/sub2api'
version = 'offline-immutable-test'
missing = f'ERROR: {image}:{version}: not found'
cases = [
    ('existing tag', 0, '', False),
    ('confirmed missing manifest', 1, missing, True),
    ('transport failure', 42, 'ERROR: registry connection timed out', False),
    ('authentication failure', 1, 'ERROR: unauthorized: authentication required', False),
    ('unrelated not-found error', 1, 'ERROR: open registry config: file not found', False),
    ('mixed missing and auth diagnostics', 1, missing + '\nERROR: unauthorized', False),
    ('empty unknown failure', 1, '', False),
]
with tempfile.TemporaryDirectory(prefix='sub2api-tag-guard-', dir=os.environ.get('TMPDIR', '/var/tmp')) as directory:
    docker = Path(directory) / 'docker'
    docker.write_text('#!/bin/sh\nprintf \'%s\\n\' "$GUARD_MESSAGE" >&2\nexit "$GUARD_STATUS"\n')
    docker.chmod(0o700)
    for name, status, message, allowed in cases:
        env = dict(os.environ, PATH=directory + os.pathsep + os.environ['PATH'],
                   IMAGE=image, VERSION=version, GUARD_STATUS=str(status), GUARD_MESSAGE=message)
        result = subprocess.run(['bash', '--noprofile', '--norc', '-e', '-o', 'pipefail', '-c', script],
                                env=env, capture_output=True, text=True, timeout=10)
        assert (result.returncode == 0) == allowed, f'{name}: unexpected guard exit {result.returncode}'
        if name == 'transport failure':
            assert result.returncode == status, 'transport failure status was not propagated'
print(f'immutable candidate-tag guard: {len(cases)} cases passed')
