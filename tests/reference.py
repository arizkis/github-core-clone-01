"""Materialize the pre-removal TypeScript oracle from repository history."""
from contextlib import contextmanager
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
REFERENCE_COMMIT = 'd9b97e1'


@contextmanager
def source_reference():
    configured = os.environ.get('FORGE_REFERENCE_ROOT')
    if configured:
        yield Path(configured).resolve()
    elif (ROOT / 'lib/graphql.ts').is_file() and (ROOT / 'main.ts').read_text().startswith('import { createApp }'):
        yield ROOT
    else:
        archive = subprocess.check_output(['git', 'archive', REFERENCE_COMMIT, 'main.ts', 'app.ts', 'lib', 'package.json'], cwd=ROOT)
        with tempfile.TemporaryDirectory(prefix='forge-typescript-reference-') as directory:
            with tarfile.open(fileobj=io.BytesIO(archive), mode='r:') as bundle:
                bundle.extractall(directory, filter='data')
            yield Path(directory)
