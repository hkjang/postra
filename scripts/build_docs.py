import os, sys, re, subprocess

def parse_inline(text):
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
  @import url('https://fonts.googleapis.com/css2?family=Noto+Sans+KR:wght@300;400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap');

  @page {{
    size: A4;
    margin: 18mm 15mm 18mm 15mm;
  }}

  body {{
    font-family: 'Noto Sans KR', -apple-system, BlinkMacSystemFont, sans-serif;
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

def convert_md_to_pdf(md_path, pdf_path, title):
    with open(md_path, 'r', encoding='utf-8') as f:
        md_content = f.read()
    
    html_content = md_to_html(md_content, title)
    tmp_html_path = md_path.replace('.md', '.tmp.html')
    
    with open(tmp_html_path, 'w', encoding='utf-8') as f:
        f.write(html_content)
        
    cmd = [
        "chromium-browser",
        "--headless",
        "--no-sandbox",
        "--disable-gpu",
        f"--print-to-pdf={pdf_path}",
        tmp_html_path
    ]
    res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if os.path.exists(tmp_html_path):
        os.remove(tmp_html_path)
        
    if res.returncode == 0 and os.path.exists(pdf_path):
        print(f"[OK] Successfully generated {pdf_path} ({os.path.getsize(pdf_path)} bytes)")
    else:
        print(f"[ERROR] Failed to generate {pdf_path}: {res.stderr.decode('utf-8')}")

if __name__ == "__main__":
    docs = [
        ("docs/USER_GUIDE.md", "docs/USER_GUIDE.pdf", "Postra 사용자 가이드"),
        ("docs/ADMIN_GUIDE.md", "docs/ADMIN_GUIDE.pdf", "Postra 관리자 가이드"),
        ("docs/EXECUTIVE_REPORT.md", "docs/EXECUTIVE_REPORT.pdf", "Postra 경영진 보고서"),
    ]
    for md, pdf, title in docs:
        if os.path.exists(md):
            convert_md_to_pdf(md, pdf, title)
        else:
            print(f"[SKIP] {md} not found")
