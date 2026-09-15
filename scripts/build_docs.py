import argparse
import html
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

def parse_inline(text):
    # Angle-bracket placeholders such as <HOST> are documentation, not HTML.
    text = html.escape(text)
    text = re.sub(r'`([^`]+)`', r'<code>\1</code>', text)
    text = re.sub(r'\*\*([^*]+)\*\*', r'<strong>\1</strong>', text)
    text = re.sub(r'\*([^*]+)\*', r'<em>\1</em>', text)
    text = re.sub(r'\[([^\]]+)\]\(([^)]+)\)', r'<a href="\2" target="_blank">\1</a>', text)
    return text

def md_to_html(md_text, title="Document"):
    lines = md_text.split('\n')
    html_lines = []
    in_code_block = False
    code_block_lines = []
    in_list = False
    list_items = []
    in_table = False
    table_rows = []
    in_blockquote = False
    bq_lines = []

    def flush_list():
        nonlocal in_list, list_items
        if in_list and list_items:
            html_lines.append("<ul>")
            for item in list_items:
                html_lines.append(f"  <li>{parse_inline(item)}</li>")
            html_lines.append("</ul>")
            list_items = []
            in_list = False

    def flush_table():
        nonlocal in_table, table_rows
        if in_table and table_rows:
            html_lines.append("<table>")
            for idx, row in enumerate(table_rows):
                cols = [c.strip() for c in row.strip('|').split('|')]
                if idx == 0:
                    html_lines.append("  <thead>\n    <tr>" + "".join(f"<th>{parse_inline(c)}</th>" for c in cols) + "</tr>\n  </thead>\n  <tbody>")
                elif idx == 1 and all(set(c) <= set(':- ') for c in cols):
                    continue # header separator
                else:
                    html_lines.append("    <tr>" + "".join(f"<td>{parse_inline(c)}</td>" for c in cols) + "</tr>")
            html_lines.append("  </tbody>\n</table>")
            table_rows = []
            in_table = False

    def flush_blockquote():
        nonlocal in_blockquote, bq_lines
        if in_blockquote and bq_lines:
            content = "<br/>".join(parse_inline(l) for l in bq_lines)
            html_lines.append(f"<blockquote>{content}</blockquote>")
            bq_lines = []
            in_blockquote = False

    for line in lines:
        raw_line = line
        line_str = line.strip()

        # Code block check
        if line_str.startswith("```"):
            if in_code_block:
                code_content = "\n".join(code_block_lines)
                html_lines.append(f"<pre><code>{code_content}</code></pre>")
                code_block_lines = []
                in_code_block = False
            else:
                flush_list()
                flush_table()
                flush_blockquote()
                in_code_block = True
            continue

        if in_code_block:
            escaped = raw_line.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
            code_block_lines.append(escaped)
            continue

        # Table check
        if line_str.startswith("|") and line_str.endswith("|"):
            flush_list()
            flush_blockquote()
            in_table = True
            table_rows.append(line_str)
            continue
        elif in_table:
            flush_table()

        # Blockquote check
        if line_str.startswith("> "):
            flush_list()
            flush_table()
            in_blockquote = True
            bq_lines.append(line_str[2:])
            continue
        elif in_blockquote:
            flush_blockquote()

        # List check
        if line_str.startswith("- ") or line_str.startswith("* "):
            flush_table()
            flush_blockquote()
            in_list = True
            list_items.append(line_str[2:])
            continue
        elif in_list and (line_str.startswith("  - ") or line_str.startswith("  * ")):
            list_items.append(line_str[4:])
            continue
        elif in_list and not (line_str.startswith("- ") or line_str.startswith("* ")):
            flush_list()

        # Headings
        if line_str.startswith("# "):
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append(f"<h1>{parse_inline(line_str[2:])}</h1>")
        elif line_str.startswith("## "):
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append(f"<h2>{parse_inline(line_str[3:])}</h2>")
        elif line_str.startswith("### "):
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append(f"<h3>{parse_inline(line_str[4:])}</h3>")
        elif line_str.startswith("#### "):
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append(f"<h4>{parse_inline(line_str[5:])}</h4>")
        elif line_str == "---":
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append("<hr/>")
        elif line_str == "":
            flush_list()
            flush_table()
            flush_blockquote()
            continue
        else:
            flush_list()
            flush_table()
            flush_blockquote()
            html_lines.append(f"<p>{parse_inline(line_str)}</p>")

    flush_list()
    flush_table()
    flush_blockquote()

    body_html = "\n".join(html_lines)
    full_html = f"""<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8">
<title>{title}</title>
<style>
  @page {{
    size: A4;
    margin: 18mm 15mm 18mm 15mm;
  }}

  body {{
    font-family: 'Noto Sans KR', 'NanumGothic', -apple-system, BlinkMacSystemFont, sans-serif;
    color: #1e293b;
    line-height: 1.7;
    font-size: 10pt;
    margin: 0;
    padding: 0;
  }}

  h1 {{
    font-size: 20pt;
    font-weight: 800;
    color: #0f172a;
    border-bottom: 3px solid #2563eb;
    padding-bottom: 8px;
    margin-top: 10px;
    margin-bottom: 20px;
    letter-spacing: -0.5px;
  }}

  h2 {{
    font-size: 14pt;
    font-weight: 700;
    color: #1e3a8a;
    border-bottom: 1.5px solid #cbd5e1;
    padding-bottom: 5px;
    margin-top: 26px;
    margin-bottom: 12px;
    letter-spacing: -0.3px;
    page-break-after: avoid;
  }}

  h3 {{
    font-size: 11.5pt;
    font-weight: 700;
    color: #0f172a;
    margin-top: 18px;
    margin-bottom: 8px;
    page-break-after: avoid;
  }}

  h4 {{
    font-size: 10.5pt;
    font-weight: 600;
    color: #334155;
    margin-top: 14px;
    margin-bottom: 6px;
  }}

  p {{
    margin-top: 0;
    margin-bottom: 10px;
    text-align: justify;
    word-break: keep-all;
  }}

  ul, ol {{
    margin-top: 0;
    margin-bottom: 12px;
    padding-left: 20px;
  }}

  li {{
    margin-bottom: 4px;
    word-break: keep-all;
  }}

  code {{
    font-family: 'JetBrains Mono', Consolas, monospace;
    font-size: 9pt;
    background-color: #f1f5f9;
    color: #0f172a;
    padding: 2px 5px;
    border-radius: 4px;
    border: 1px solid #e2e8f0;
  }}

  pre {{
    background-color: #0f172a;
    color: #f8fafc;
    padding: 12px 16px;
    border-radius: 6px;
    overflow-x: auto;
    margin-top: 10px;
    margin-bottom: 14px;
    page-break-inside: avoid;
  }}

  pre code {{
    background-color: transparent;
    color: inherit;
    padding: 0;
    border: none;
    font-size: 8.5pt;
    line-height: 1.45;
  }}

  blockquote {{
    margin: 12px 0;
    padding: 10px 16px;
    background-color: #eff6ff;
    border-left: 4px solid #3b82f6;
    color: #1e40af;
    border-radius: 0 6px 6px 0;
    font-size: 9.5pt;
  }}

  table {{
    width: 100%;
    border-collapse: collapse;
    margin-top: 12px;
    margin-bottom: 16px;
    font-size: 9pt;
    page-break-inside: avoid;
  }}

  th {{
    background-color: #f1f5f9;
    color: #0f172a;
    font-weight: 700;
    text-align: left;
    padding: 8px 10px;
    border: 1px solid #cbd5e1;
  }}

  td {{
    padding: 7px 10px;
    border: 1px solid #cbd5e1;
    color: #334155;
  }}

  tr:nth-child(even) td {{
    background-color: #f8fafc;
  }}

  hr {{
    border: none;
    border-top: 1px solid #e2e8f0;
    margin: 20px 0;
  }}

  a {{
    color: #2563eb;
    text-decoration: none;
  }}
</style>
</head>
<body>
{body_html}
</body>
</html>
"""
    return full_html

def convert_md_to_pdf(md_path, pdf_path, title, chrome=None, renderer="chromium"):
    chrome = chrome or os.environ.get("POSTRA_CHROME") or os.environ.get("CHROME") or shutil.which("chromium-browser") or shutil.which("chromium")
    if not chrome:
        raise RuntimeError("Chromium not found; set POSTRA_CHROME or CHROME to a local executable")
    with open(md_path, 'r', encoding='utf-8') as f:
        md_content = f.read()
    html_content = md_to_html(md_content, title)
    # Relative documentation links must not expose the temporary build path in
    # distributed PDFs. This only sets link destinations; printing is offline.
    html_content = html_content.replace('<meta charset="utf-8">', '<meta charset="utf-8">\n<base href="https://github.com/hkjang/postra/blob/main/docs/">', 1)
    pdf_path = Path(pdf_path).resolve()
    # Render beside the destination so replacing an existing PDF is atomic.
    # A missing browser, timeout or failed print must preserve the old artifact.
    with tempfile.TemporaryDirectory(prefix=".postra-docs-", dir=pdf_path.parent) as temporary, tempfile.TemporaryDirectory(prefix="postra-docs-browser-") as profile:
        temporary = Path(temporary)
        temporary_html = temporary / "document.html"
        temporary_pdf = temporary / "document.pdf"
        temporary_html.write_text(html_content, encoding="utf-8")
        cmd = [chrome, "--headless", "--no-sandbox", "--disable-gpu",
               "--disable-background-networking", "--no-pdf-header-footer",
               f"--user-data-dir={profile}",
               f"--print-to-pdf={temporary_pdf}", temporary_html.as_uri()]
        if renderer == "playwright":
            cmd = ["node", str(Path(__file__).with_name("print_pdf.cjs")), chrome, temporary_html.as_uri(), str(temporary_pdf)]
        res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        if res.returncode != 0 or not temporary_pdf.is_file() or temporary_pdf.stat().st_size == 0:
            raise RuntimeError(f"Failed to generate {pdf_path.name}: {res.stderr.decode('utf-8', errors='replace')[-2000:]}")
        os.replace(temporary_pdf, pdf_path)
    print(f"[OK] Successfully generated {pdf_path.name} ({pdf_path.stat().st_size} bytes)")

if __name__ == "__main__":
    docs = {"USER_GUIDE": "Postra 사용자 가이드", "ADMIN_GUIDE": "Postra 관리자 가이드", "EXECUTIVE_REPORT": "Postra 경영진 보고서"}
    parser = argparse.ArgumentParser(description="Print selected guides with local Chromium and fonts; no external font requests")
    parser.add_argument("documents", nargs="*", help="Document names: USER_GUIDE, ADMIN_GUIDE, EXECUTIVE_REPORT (default: all)")
    parser.add_argument("--renderer", choices=["chromium", "playwright"], default="chromium", help="Use Playwright from web/node_modules if Chromium CLI printing is unavailable")
    args = parser.parse_args()
    selected = args.documents or list(docs)
    if any(name not in docs for name in selected):
        parser.error("unknown document; select USER_GUIDE, ADMIN_GUIDE or EXECUTIVE_REPORT")
    root = Path(__file__).resolve().parents[1]
    for name in selected:
        convert_md_to_pdf(root / "docs" / (name + ".md"), root / "docs" / (name + ".pdf"), docs[name], renderer=args.renderer)
