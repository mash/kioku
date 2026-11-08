#!/usr/bin/env python3
"""Build a self-contained Codex marketplace and npm tarball; never publish."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
TARGETS = ('darwin-amd64', 'darwin-arm64', 'linux-amd64', 'linux-arm64')
BUILDINFO = 'github.com/mash/kioku/internal/buildinfo'


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise ValueError(f'canonical skill changed; review Codex adaptation: {old[:80]!r}')
    return text.replace(old, new, 1)


def adapt_skill(text):
    start = text.index('プラグインでは、CLI のコマンド名として')
    end = text.index('- **local mode**', start)
    text = text[:start] + '''Codex plugin では、SessionStart フックが示した同梱 CLI の絶対パスを使う。
フックの通知がない場合は、この SKILL.md の実際の位置から `../../bin/kioku` を解決する。
本文・参照ファイルの `kioku` はその絶対パスに置き換え、シェルに適切に引用して実行する。
コマンド名に変数・コマンド置換を使わず、CLI の絶対パスから始め、`-C` で対象を指定する。
開始時に `version` を1回実行する。破損・欠損・checksum不一致なら再インストールを案内して止める。
PATH の別の CLI や MCP で package の破損を回避しない。
フックが未信頼なら、Codex の hook 信頼画面で確認する方法を案内する。信頼設定は自動編集しない。
この plugin は CLI を同梱する。グローバルインストール・ダウンロード・PATH 設定は不要。
既に選択済みの対象と取得状態は再開・圧縮後も維持し、開始通知だけを理由に再 pull しない。

''' + text[end:]
    text = replace_once(text,
        'chat と Cowork はプラグインの Connectors タブ、Claude Code は `/mcp` を案内する。',
        'Codex の接続設定を案内する。この同梱 plugin 自体には MCP 接続は含まれない。')
    text = replace_once(text, 'Claude Code の例は `mcp__plugin_kioku_kioku__wiki_read`。',
                        '接続済みツールから Kioku の wiki_read を検索する。ツール名を推測して呼ばない。')
    return text


def adapt_selection(text):
    lines = text.splitlines()
    for i, line in enumerate(lines):
        if line.startswith('3. 有効な認証がなく、`CLAUDE_CODE_REMOTE_SESSION_ID`'):
            lines[i] = '3. 有効な認証がなく、ユーザーが connector login を選んだ場合は [connector.md](connector.md) に従う。'
        if line.startswith('4. 有効な認証がなく、ローカル端末の場合は、'):
            lines[i] = ('4. 有効な認証がない場合は、人に端末でのログインを頼み、完了を待つ。'
                        '実際の同梱 CLI の絶対パス・P・SERVER を適切に引用して '
                        '`/absolute/plugin/bin/kioku -C /absolute/project --server https://kioku.magicians.ee login` '
                        'に相当するコマンドを示す。ブラウザ認証は人が行う。'
                        '認証ファイルを読んだりコピーさせたりしない。端末で操作できない場合は '
                        '[connector.md](connector.md) の選択肢を説明する。'
                        '人のログイン後は「人のログイン後の全プロジェクト取得」へ進む。')
        if line.startswith('- CLI のパスに CLI が見つからない場合は、'):
            lines[i] = '- 同梱 CLI の欠損・破損なら再インストールを案内して止める。PATH や MCP で回避しない。'
    text = '\n'.join(lines) + '\n'
    start = text.index('## プラグインなし（Codex など）')
    end = text.index('## 記録の共有範囲', start)
    return text[:start] + text[end:]


def check_binary(path, target, version, commit):
    if not path.is_file() or path.is_symlink():
        raise ValueError(f'missing or symlinked binary: {path}')
    metadata = subprocess.check_output(['go', 'version', '-m', str(path)], text=True)
    os_name, arch = target.split('-')
    for setting in (f'GOOS={os_name}', f'GOARCH={arch}'):
        if not re.search(r'\b' + re.escape(setting) + r'(?:\s|$)', metadata):
            raise ValueError(f'{target}: wrong build target ({setting})')
    receipt = json.loads(path.with_name('build.json').read_text())
    expected = {'version': version, 'commit': commit, 'target': target,
                'sha256': hashlib.sha256(path.read_bytes()).hexdigest()}
    if receipt != expected:
        raise ValueError(f'{target}: build receipt does not match binary/package')
    return expected['sha256']


def archive(source, output, prefix):
    # Explicit members prevent accidental traversal through directory symlinks.
    with tarfile.open(output, 'w:gz', format=tarfile.PAX_FORMAT) as tar:
        for path in sorted(source.rglob('*')):
            if path.is_symlink():
                raise ValueError(f'package contains symlink: {path}')
            tar.add(path, arcname=str(Path(prefix) / path.relative_to(source)), recursive=False)


def package(binary_root, output, version, commit, npm_name):
    canonical = json.loads((ROOT / 'plugin/.claude-plugin/plugin.json').read_text())
    if not re.fullmatch(r'v\d+\.\d+\.\d+', version) or version != 'v' + canonical['version']:
        raise ValueError('version must match plugin/.claude-plugin/plugin.json')
    if not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise ValueError('commit must be a full lower-case SHA')
    if not re.fullmatch(r'(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*', npm_name):
        raise ValueError('invalid npm package name')
    if output.exists():
        raise ValueError(f'refusing to overwrite {output}')
    verified = {target: check_binary(binary_root / target.replace('-', '_') / 'kioku',
                                     target, version, commit) for target in TARGETS}
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.codex-package-', dir=output.parent) as tmp:
        stage = Path(tmp) / 'output'
        market = stage / 'marketplace'
        plugin = market / 'plugins/kioku'
        plugin.mkdir(parents=True)
        for name in ('bin', 'hooks', 'prompts'):
            shutil.copytree(ROOT / 'codex-plugin' / name, plugin / name)
        # Only Markdown instruction files belong in the runtime package.
        source = ROOT / 'plugin/skills/kioku'
        for path in source.rglob('*.md'):
            if path.is_symlink():
                raise ValueError(f'skill symlink not allowed: {path}')
            text = path.read_text()
            if path.name == 'SKILL.md':
                text = adapt_skill(text)
            elif path.name == 'selection.md':
                text = adapt_selection(text)
            text = text.replace('Claude', 'Codex')
            dest = plugin / 'skills/kioku' / path.relative_to(source)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(text)
        extension = {
            'hooks': './hooks/hooks.json', 'requires_local_executor': True,
            'interface': {'displayName': 'Kioku', 'shortDescription': 'Shared project context with a bundled CLI'},
        }
        manifest = {'$schema': 'https://agent-plugins.org/schemas/1.0.0/plugin.schema.json',
                    'name': 'kioku', 'version': version[1:],
                    'description': 'Kioku shared project wiki with bundled CLI and session context',
                    'extensions': {'com.openai': extension}}
        # Use the documented Codex compatibility format. The tested desktop CLI
        # discovers skills but drops hooks with the portable schema manifest.
        # Do not emit both formats until hook discovery is verified on the target.
        write_json(plugin / '.codex-plugin/plugin.json', {
            'name': manifest['name'], 'version': manifest['version'],
            'description': manifest['description'], 'skills': './skills/', **extension})
        write_json(plugin / 'package.json', {'name': npm_name, 'version': version[1:],
                   'description': manifest['description'], 'license': 'MIT',
                   'files': ['.codex-plugin', 'skills', 'bin', 'libexec', 'hooks',
                             'prompts', 'checksums.json', 'README.md', 'LICENSE']})
        shutil.copyfile(ROOT / 'LICENSE', plugin / 'LICENSE')
        shutil.copyfile(ROOT / 'codex-plugin/README.md', plugin / 'README.md')
        sums = {}
        for target in TARGETS:
            src = binary_root / target.replace('-', '_') / 'kioku'
            dest = plugin / 'libexec' / target / 'kioku'
            dest.parent.mkdir(parents=True)
            shutil.copyfile(src, dest)
            dest.chmod(0o755)
            digest = hashlib.sha256(dest.read_bytes()).hexdigest()
            if digest != verified[target]:
                raise ValueError(f'{target}: binary changed after verification')
            dest.with_name('SHA256SUM').write_text(digest + '\n')
            sums[str(dest.relative_to(plugin))] = digest
        write_json(plugin / 'checksums.json', {'version': version, 'commit': commit, 'sha256': sums})
        write_json(market / '.agents/plugins/marketplace.json', {
            'name': 'kioku', 'interface': {'displayName': 'Kioku'}, 'plugins': [{
                'name': 'kioku', 'source': {'source': 'local', 'path': './plugins/kioku'},
                'policy': {'installation': 'AVAILABLE', 'authentication': 'ON_USE'},
                'category': 'Productivity'}]})
        archive(market, stage / f'kioku-codex_{version}.tar.gz', 'kioku-codex')
        archive(plugin, stage / f'kioku-codex-npm_{version}.tgz', 'package')
        stage.rename(output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binaries', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--npm-name', default='kioku-codex-plugin', help='Unpublished name; ownership must be verified before publishing')
    args = parser.parse_args()
    try:
        package(args.binaries.resolve(), args.output.resolve(), args.version, args.commit, args.npm_name)
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        parser.exit(1, f'Packaging failed: {exc}\n')
