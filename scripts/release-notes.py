#!/usr/bin/env python3
"""Print the changelog section of one release, or fail when there is none."""
import argparse
import re
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('version')
parser.add_argument('--changelog', type=Path, default=Path('CHANGELOG.md'))
args = parser.parse_args()
source = args.changelog.read_text(encoding='utf-8')
heading = re.search(r'^## ' + re.escape(args.version) + r' - \d{4}-\d{2}-\d{2}$', source, re.M)
if heading is None:
    parser.exit(1, f'{args.changelog} has no section "## {args.version} - YYYY-MM-DD"\n')
notes = re.split(r'^## ', source[heading.end():], maxsplit=1, flags=re.M)[0]
print(notes.strip())
