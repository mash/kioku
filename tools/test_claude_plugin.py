#!/usr/bin/env python3
"""Run offline integration against the plugin's actual hook configuration."""
import argparse
import os
from pathlib import Path
import subprocess
p=argparse.ArgumentParser()
p.add_argument('--binary',required=True)
p.add_argument('--plugin',required=True)
a=p.parse_args()
env=dict(os.environ,KIOKU_TEST_BINARY=str(Path(a.binary).resolve()))
subprocess.run(['python3','-m','unittest','discover','-s','tests'],cwd=Path(a.plugin).resolve(),env=env,check=True)
