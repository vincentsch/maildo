#!/usr/bin/env python3
"""Check local Markdown links, image sources and heading anchors."""
import re
from pathlib import Path
from urllib.parse import unquote, urlsplit

root = Path(__file__).resolve().parents[1]
files = [root / 'README.md', root / 'AGENTS.md', root / 'CONTRIBUTING.md',
         root / 'SECURITY.md', root / 'RELEASE.md', root / 'CHANGELOG.md',
         root / 'skills/maildo/SKILL.md', *sorted((root / 'docs').rglob('*.md'))]
failures = []
count = 0
for path in files:
    source = path.read_text()
    source = re.sub(r'```.*?```', '', source, flags=re.S)
    links = re.findall(r'!?\[[^\]]*\]\(([^) ]+)(?: +"[^"]*")?\)', source)
    links += re.findall(r'(?:href|src|srcset)="([^"]+)"', source)
    for link in links:
        url = urlsplit(link)
        if url.scheme or url.netloc:
            continue
        target = (path.parent / unquote(url.path)).resolve() if url.path else path
        count += 1
        if not target.exists():
            failures.append(f'{path.relative_to(root)}: missing {link}')
        elif url.fragment and target.suffix == '.md':
            anchors = set()
            seen = {}
            for heading in re.findall(r'^#{1,6}\s+(.+)', target.read_text(), re.M):
                slug = re.sub(r'[^\w\- ]', '', heading.lower()).replace(' ', '-')
                n = seen.get(slug, 0)
                seen[slug] = n + 1
                anchors.add(slug + (f'-{n}' if n else ''))
            if unquote(url.fragment) not in anchors:
                failures.append(f'{path.relative_to(root)}: missing anchor {link}')
if failures:
    raise SystemExit('\n'.join(failures))
print(f'Checked {count} local links and anchors across {len(files)} Markdown files.')
