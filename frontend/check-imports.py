#!/usr/bin/env python3
"""前端 import 门禁：页面使用了 ui.js 的组件却没 import → 运行期 ReferenceError。

真机事故：receive.js 用了 kv() 但未 import，导致「接收」页直接报错（看起来像空白）。
用法：check-imports.py <webui 目录>      （默认 frontend/）
"""
import re
import sys
import os
import glob

root = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__))
ui = os.path.join(root, "assets", "ui.js")
ui_src = open(ui, encoding="utf-8").read()
exports = set(re.findall(r"export (?:const|function) (\w+)", ui_src))

bad = []
for f in sorted(glob.glob(os.path.join(root, "assets", "pages", "*.js"))):
    s = open(f, encoding="utf-8").read()
    m = re.search(r"import \{([^}]*)\} from '\.\./ui\.js", s)
    imported = {x.strip().split(" as ")[0] for x in (m.group(1).split(",") if m else []) if x.strip()}
    used = set(re.findall(r"(?<![\w.])([a-z][A-Za-z0-9_]*)\(]", s))
    missing = sorted((used & exports) - imported)
    if missing:
        bad.append((os.path.relpath(f, root), missing))

if bad:
    print("✗ 前端 import 检查未通过：")
    for name, missing in bad:
        print(f"   {name}: 使用了未导入的 {', '.join(missing)}")
    sys.exit(1)
print(f"✓ 前端 import 检查通过（{len(glob.glob(os.path.join(root, 'assets', 'pages', '*.js')))} 个页面）")
