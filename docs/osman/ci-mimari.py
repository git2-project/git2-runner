# Generates ci-mimari.html: the CI lane explained step by step, in Turkish.
# Usage: python docs/osman/ci-mimari.py docs/osman/ci-mimari.html
# Edit the data (BOX, ARROWS, STEPS, ...) here and run it again; do not edit the HTML by hand.
import html
import math
import sys

E = html.escape

# ---------------------------------------------------------------- the map
# One server map, drawn again for every step with the active parts lit.
H = 38
BOX = {
    #        x    y    w    title                    sub                     kind
    "dev":  (14, 108, 96, "Geliştirici", "git · tarayıcı", "other"),
    "gw":   (142, 54, 112, "gateway", "HTTPS :443", "other"),
    "core": (142, 124, 112, "git2 çekirdek", "dash/admin-api", "other"),
    "git":  (290, 54, 112, "git-svc", ":9090 · :9080", "other"),
    "ci":   (290, 124, 112, "ci-svc", ":9090 · :9091", "ci"),
    "pg":   (500, 124, 100, "PostgreSQL", "kalıcı kayıt", "db"),
    "rd":   (500, 186, 100, "Redis", "uyandırma zili", "rd"),
    "run":  (640, 54, 196, "git2-runner", "iş ister, koşturur", "run"),
    "dock": (640, 124, 196, "Docker + iş konteyneri", "git2-job-<id>", "run"),
}
ZONES = [
    (6, 22, 112, 208, "BİLGİSAYAR"),
    (130, 22, 482, 208, "SUNUCU 1 · git2 sunucusu (Docker Compose)"),
    (624, 22, 230, 208, "SUNUCU 2 · build host"),
]
VIEW = (860, 236)


def rect(k):
    x, y, w = BOX[k][:3]
    return x, y, w, H


def center(k):
    x, y, w, h = rect(k)
    return x + w / 2, y + h / 2


def exit_point(k, px, py, dx, dy):
    """Where a ray from (px, py) inside box k, going (dx, dy), leaves it."""
    x, y, w, h = rect(k)
    ts = []
    if dx > 0:
        ts.append((x + w - px) / dx)
    elif dx < 0:
        ts.append((x - px) / dx)
    if dy > 0:
        ts.append((y + h - py) / dy)
    elif dy < 0:
        ts.append((y - py) / dy)
    t = min(t for t in ts if t >= 0)
    return px + dx * t, py + dy * t


def arrow_path(a, b, off=0.0, start=None, end=None):
    ax, ay = start if start else center(a)
    bx, by = end if end else center(b)
    dx, dy = bx - ax, by - ay
    L = math.hypot(dx, dy)
    nx, ny = -dy / L, dx / L
    ax, ay, bx, by = ax + nx * off, ay + ny * off, bx + nx * off, by + ny * off
    sx, sy = (ax, ay) if start else exit_point(a, ax, ay, dx, dy)
    ex, ey = (bx, by) if end else exit_point(b, bx, by, -dx, -dy)
    # leave room for the arrow head
    ux, uy = (ex - sx) / math.hypot(ex - sx, ey - sy), (ey - sy) / math.hypot(ex - sx, ey - sy)
    ex, ey = ex - ux * 3, ey - uy * 3
    sx, sy = sx + ux * 2, sy + uy * 2
    return (sx, sy, ex, ey)


# Protocol of each arrow decides its colour.
PROTO = {
    "http": "HTTPS / Git",
    "grpc": "gRPC çağrısı",
    "sql": "PostgreSQL",
    "redis": "Redis zili",
    "docker": "Docker Engine API",
}

# Named arrows: (from, to, protocol, offset, start, end)
ARROWS = {
    "push":    ("dev", "gw", "http", 0, None, None),
    "gw_git":  ("gw", "git", "http", 0, None, None),
    "git_pg":  ("git", "pg", "sql", 0, None, None),
    "deliver": ("git", "ci", "grpc", -16, None, None),
    "getblob": ("ci", "git", "grpc", -16, None, None),
    "ci_pg":   ("ci", "pg", "sql", 0, None, None),
    "ci_rd":   ("ci", "rd", "redis", 0, None, None),
    "rd_ci":   ("rd", "ci", "redis", 0, (476, 214), (402, 158)),
    "run_ci":  ("run", "ci", "grpc", 0, (640, 64), (380, 124)),
    "ci_run":  ("ci", "run", "grpc", 0, (402, 140), (640, 80)),
    "run_dock": ("run", "dock", "docker", 0, None, None),
    "gw_core": ("gw", "core", "http", 0, None, None),
    "core_ci": ("core", "ci", "grpc", 0, None, None),
}


def svg_box(k, on):
    x, y, w, h = rect(k)
    title, sub, kind = BOX[k][3:]
    cls = f"bx k-{kind} {'on' if on else 'off'}"
    return (f'<g class="{cls}"><rect x="{x}" y="{y}" width="{w}" height="{h}" rx="7"/>'
            f'<text class="t" x="{x + w / 2}" y="{y + 16}" text-anchor="middle">{E(title)}</text>'
            f'<text class="s" x="{x + w / 2}" y="{y + 30}" text-anchor="middle">{E(sub)}</text></g>')


def svg_zones():
    out = []
    for x, y, w, h, label in ZONES:
        out.append(f'<rect class="zone" x="{x}" y="{y}" width="{w}" height="{h}" rx="10"/>')
        out.append(f'<text class="zl" x="{x + 8}" y="{y + h - 7}">{E(label)}</text>')
    return "".join(out)


def svg_arrow(name, label=None, lx=None, ly=None, anchor="middle"):
    a, b, proto, off, start, end = ARROWS[name]
    sx, sy, ex, ey = arrow_path(a, b, off, start, end)
    s = (f'<path class="ar p-{proto}" d="M{sx:.1f},{sy:.1f} L{ex:.1f},{ey:.1f}" '
         f'marker-end="url(#m-{proto})"/>')
    if label:
        if lx is None:
            lx, ly = (sx + ex) / 2, (sy + ey) / 2 - 8
        s += f'<text class="al p-{proto}" x="{lx}" y="{ly}" text-anchor="{anchor}">{E(label)}</text>'
    return s


def svg_badge(name, text, t=0.5):
    a, b, proto, off, start, end = ARROWS[name]
    sx, sy, ex, ey = arrow_path(a, b, off, start, end)
    x, y = sx + (ex - sx) * t, sy + (ey - sy) * t
    w = 8 + 7 * len(text)
    return (f'<g class="badge p-{proto}"><rect x="{x - w / 2:.1f}" y="{y - 9:.1f}" width="{w}" height="18" rx="9"/>'
            f'<text x="{x:.1f}" y="{y + 4:.1f}" text-anchor="middle">{E(text)}</text></g>')


def map_svg(on, arrows, aria, badges=None):
    w, h = VIEW
    parts = [f'<svg viewBox="0 0 {w} {h}" role="img" aria-label="{E(aria)}">', svg_zones()]
    for k in BOX:
        parts.append(svg_box(k, k in on))
    for a in arrows:
        parts.append(svg_arrow(*a) if isinstance(a, tuple) else svg_arrow(a))
    for name, text, *t in (badges or []):
        parts.append(svg_badge(name, text, *(t or [0.5])))
    parts.append("</svg>")
    return "".join(parts)


MARKERS = "".join(
    f'<marker id="m-{p}" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
    f'<path class="mk p-{p}" d="M0,0 L10,5 L0,10 z"/></marker>'
    for p in PROTO
)

# ---------------------------------------------------------------- the steps
# (title, on-boxes, arrows, what happens, chips, status)
STEPS = [
    ("Geliştirici <code>git push</code> yapar",
     ["dev", "gw", "git"],
     [("push", "git push", 62, 100), ("gw_git", "Git (Smart HTTP)", 272, 47)],
     ["Git, yeni commit'leri HTTPS ile gönderir. Dışarıya açık <b>tek kapı gateway'dir</b> (443).",
      "gateway kullanıcı adı ve şifreyi kontrol eder, isteği içerideki git-svc'ye iletir. Kimin ittiği bir başlıkla gider.",
      "git-svc commit'leri diske, depo klasörüne yazar ve branch'i yeni commit'e taşır."],
     [("yazılan", "disk: depo klasörü"), ("protokol", "HTTPS")], "main"),
    ("git-svc olayı <b>giden kutusuna</b> (outbox) yazar",
     ["git", "pg"],
     [("git_pg", "git_svc.outbox", 452, 100, "start")],
     ["Branch taşındıktan hemen sonra git-svc Postgres'e bir satır ekler: <code>git.v1.RepositoryPushed</code>.",
      "Satırda: hangi depo, kim itti, hangi ref eski SHA'dan yeni SHA'ya geçti.",
      "Neden önce bir tabloya? ci-svc o an kapalı olsa bile olay kaybolmasın diye. Kutu kalıcıdır."],
     [("yazılan", "git_svc.outbox"), ("protokol", "SQL")], "codeplane"),
    ("Relay olayı ci-svc'ye <b>teslim eder</b>",
     ["git", "ci"],
     [("deliver", "EventSink.Deliver", 368, 112, "start")],
     ["git-svc'nin içindeki relay <b>her saniye</b> outbox'a bakar ve gönderilmemiş satırları alır.",
      "Her satır için ci-svc'nin <code>EventSink.Deliver</code> fonksiyonunu ağ üzerinden çağırır (gRPC, iç ağ, port 9090).",
      "ci-svc &laquo;tamam&raquo; derse satır &laquo;gönderildi&raquo; olur. Cevap gelmezse artan aralıklarla tekrar dener ve olayı asla silmez."],
     [("süre", "≤ 1 sn"), ("protokol", "gRPC :9090")], "pr49"),
    ("ci-svc olayı <b>gelen kutusuna</b> (inbox) yazar, zili çalar",
     ["ci", "pg", "rd"],
     [("ci_pg", "ci_svc.inbox", 451, 137), ("ci_rd", "ci:inbox:wake", 492, 212, "end")],
     ["Deliver <b>sadece kaydeder</b>: <code>ci_svc.inbox</code>'a bir satır. <code>event_id</code> benzersizdir; aynı olay ikinci kez gelirse hiçbir şey değişmez.",
      "Sonra Redis'te <code>ci:inbox:wake</code> zilini çalar. Zil veri taşımaz, yalnızca &laquo;bakın, yeni bir şey var&raquo; der.",
      "Cevap birkaç milisaniyede döner. Asıl iş bir sonraki adımda yapılır."],
     [("yazılan", "ci_svc.inbox"), ("süre", "~5 ms")], "pr49"),
    ("İşçi <code>.git2-ci.yml</code> dosyasını okur",
     ["ci", "git", "pg"],
     [("ci_pg", "kirala · 2 dk", 451, 137), ("getblob", "GetBlob(.git2-ci.yml)", 324, 112, "end")],
     ["ci-svc'deki inbox işçisi zili duyar; zil kaybolsa bile 5 saniyede bir zaten bakar.",
      "Satırı <b>2 dakikalık kirayla</b> alır. Kira süresince ci-svc'nin başka bir kopyası aynı satırı almaz.",
      "Olaydaki her branch ve tag için git-svc'ye sorar: &laquo;bu commit'te <code>.git2-ci.yml</code> var mı?&raquo; (<code>CommitService.GetBlob</code>).",
      "Dosya yoksa bu commit için CI yoktur ve iş biter. git-svc kapalıysa satır bekler ve 5 sn, 10 sn, 20 sn… en çok 5 dk arayla yeniden denenir."],
     [("protokol", "gRPC → git-svc :9090"), ("bekleme", "5 sn yoklama")], "pr49"),
    ("Pipeline ve işler oluşur",
     ["ci", "pg"],
     [("ci_pg", "pipeline + jobs", 451, 137)],
     ["YAML çözülür: aşamalar, işler, image, script, etiketler, süre sınırı. Dosya hatalıysa pipeline doğrudan <b>FAILED</b> olur ve hata mesajı içine yazılır.",
      "<b>Tek bir Postgres işleminde</b> (transaction): bir <code>pipelines</code> satırı, her iş için bir <code>jobs</code> satırı (durum: <i>created</i>), ilk aşamanın işleri <i>pending</i> (kuyrukta), outbox'a <code>PipelineCreated</code>.",
      "Ya hepsi yazılır ya hiçbiri. Yarım pipeline olmaz.",
      "Anahtar <code>&lt;event_id&gt;:&lt;ref&gt;</code> olduğu için aynı push iki kez işlense bile ikinci pipeline açılmaz."],
     [("yazılan", "pipelines · jobs · outbox"), ("kural", "hepsi ya da hiçbiri")], "main"),
    ("Bekleyen runner istekleri <b>uyandırılır</b>",
     ["ci", "rd"],
     [("ci_rd", "zil: ci:jobs:wake", 492, 212, "end")],
     ["ci-svc Redis'te <code>ci:jobs:wake</code> zilini çalar. Zili ci-svc'nin kendisi ve varsa diğer kopyaları dinler.",
      "Zil, o anda &laquo;iş var mı?&raquo; diye bekleyen runner isteklerini uyandırır. İş milisaniyeler içinde verilir.",
      "<b>Runner Redis'i hiç görmez.</b> Build host'lar güvenilmez kabul edilir; yalnızca ci-svc'nin 9091 portunu görür.",
      "Redis kapalıysa bekleyen istekler zaten 3 saniyede bir Postgres'e bakar; iş biraz gecikir ama kaybolmaz."],
     [("kanal", "ci:jobs:wake"), ("Redis yoksa", "≤ 3 sn gecikme")], "main"),
    ("Runner işi alır (<i>long poll</i>)",
     ["run", "ci", "pg"],
     [("run_ci", "RequestJob ⇄ iş + sırlar", 628, 50, "end"), ("ci_pg", "SKIP LOCKED", 451, 137)],
     ["Runner build host'tan dışarıya doğru bağlanır ve sorar: &laquo;bana iş var mı?&raquo; (<code>RequestJob</code>, port 9091, kendi token'ı ile).",
      "İş yoksa ci-svc cevabı <b>25 saniyeye kadar bekletir</b>. Buna <i>long poll</i> denir: runner sürekli sormak zorunda kalmaz.",
      "ci-svc, runner'ın etiketlerine uyan en eski <i>pending</i> işi <code>FOR UPDATE SKIP LOCKED</code> ile seçer. İki runner aynı işi asla alamaz.",
      "İş <i>running</i> olur ve <b>60 saniyelik kira</b> başlar. Cevapta image, script, değişkenler ve maskelenecek sırlar vardır."],
     [("protokol", "gRPC :9091 + token"), ("kira", "60 sn")], "main"),
    ("Runner konteyneri kurar, script'i çalıştırır",
     ["run", "dock"],
     [("run_dock", "Docker API", 746, 112, "start")],
     ["Image yoksa çekilir. Sonra işe özel bir volume, bir ağ ve bir konteyner kurulur; üçünün adı da <code>git2-job-&lt;job_id&gt;</code> olur. Ayrıntılar <a href=\"#konteyner\">bölüm 6</a>'da.",
      "Script <code>/bin/sh -c</code> ile ve <code>set -e</code> ile çalışır: her satır logda yeşil <code>$ komut</code> olarak görünür ve ilk hatada durur.",
      "Konteyner sınırlıdır: 2048 MB bellek, 2 CPU, 1024 süreç. Ayrıcalığı yoktur ve Docker soketini göremez."],
     [("API", "Docker Engine (moby)"), ("çalışma klasörü", "/workspace")], "main"),
    ("Loglar ve nabız (<i>heartbeat</i>)",
     ["run", "ci", "pg", "rd"],
     [("run_ci", "AppendLog · UpdateJob", 628, 50, "end"), ("ci_pg", "log parçaları", 451, 137),
      ("ci_rd", "ci:log:<job>", 492, 212, "end")],
     ["Runner çıktıyı yarım saniyelik parçalar halinde gönderir (<code>AppendLog</code>). <b>Sırlar gönderilmeden önce <code>***</code> ile maskelenir.</b>",
      "Her parça bayt konumuyla gelir; tekrar göndermek güvenlidir. Log en çok 4 MiB olabilir.",
      "Her 15 saniyede bir <code>UpdateJob(RUNNING)</code> gönderilir: kira 60 sn uzar, cevapta &laquo;iptal edildi mi?&raquo; bilgisi gelir. Artık konteynerin CPU ve bellek ölçümü de bu nabızla gider.",
      "Her log parçasından sonra <code>ci:log:&lt;job_id&gt;</code> zili çalar ve Dash'te canlı log izleyen herkes yeni satırları hemen görür."],
     [("yazılan", "job_log_chunks"), ("nabız", "15 sn")], "main"),
    ("İş biter, sıradaki aşama başlar",
     ["run", "ci", "pg", "rd"],
     [("run_ci", "UpdateJob(SUCCESS / FAILED)", 628, 50, "end"), ("ci_pg", "sonuç + sıradaki", 451, 137),
      ("ci_rd", "ci:jobs:wake", 492, 212, "end")],
     ["Runner önce logun kalanını, sonra sonucu gönderir: <b>SUCCESS</b> (çıkış kodu 0) ya da <b>FAILED</b> (script hatası, süre aşımı, image çekilemedi…).",
      "Bir aşamanın bütün işleri başarılıysa sıradaki aşamanın işleri <i>pending</i> olur ve adım 7'ye dönülür. Biri başarısızsa sonraki aşamalar <i>skipped</i> olur.",
      "Son iş de bitince pipeline SUCCESS ya da FAILED olur; outbox'a <code>JobFinished</code> ve <code>PipelineFinished</code> yazılır.",
      "Runner Docker'da kurduğu her şeyi siler ve yeniden &laquo;iş var mı?&raquo; diye sormaya başlar."],
     [("yazılan", "jobs · pipelines · outbox")], "main"),
    ("Dash sonucu gösterir",
     ["dev", "gw", "core", "ci"],
     [("push", "tarayıcı", 62, 100), ("gw_core", "sayfa", 186, 112, "end"), ("core_ci", "GetPipeline · StreamJobLog", 272, 182)],
     ["Tarayıcı yalnızca gateway üzerinden dash-api ile konuşur. <b>Tarayıcı ci-svc'yi hiç görmez.</b>",
      "dash-api önce kullanıcının yetkisine bakar, sonra ci-svc'yi çağırır: <code>GetPipeline</code> ve <code>ListPipelines</code> durumları, <code>StreamJobLog</code> ise canlı logu verir.",
      "Merge isteğindeki ✓ / ✗ işareti için olayların dash-api'ye gitmesi gerekiyor. Bu henüz yapılmadı (#48)."],
     [("protokol", "HTTPS → gRPC :9090")], "main"),
]

STATUS = {
    "main": ("main'de", "st-main"),
    "pr49": ("PR #49 · onaylı, merge bekliyor", "st-pr"),
    "codeplane": ("codeplane v0.3.0", "st-main"),
}

# ---------------------------------------------------------------- pieces
ICONS = {
    "sunucu": '<rect x="14" y="6" width="44" height="44" rx="4" class="ic-s"/><rect x="20" y="12" width="32" height="8" rx="2" class="ic-f"/><rect x="20" y="24" width="32" height="8" rx="2" class="ic-f"/><rect x="20" y="36" width="32" height="8" rx="2" class="ic-f"/><circle cx="47" cy="16" r="1.8" class="ic-a"/><circle cx="47" cy="28" r="1.8" class="ic-a"/><circle cx="47" cy="40" r="1.8" class="ic-a"/>',
    "konteyner": '<rect x="8" y="14" width="56" height="30" rx="2" class="ic-s"/><path d="M18 14v30M28 14v30M38 14v30M48 14v30" class="ic-l"/><path d="M8 8h56" class="ic-l"/>',
    "port": '<rect x="6" y="10" width="38" height="36" rx="5" class="ic-s"/><text x="25" y="32" text-anchor="middle" class="ic-t">svc</text><rect x="44" y="22" width="10" height="12" class="ic-f"/><path d="M54 28h12" class="ic-a2"/><text x="60" y="20" text-anchor="middle" class="ic-t2">:9090</text>',
    "rpc": '<rect x="4" y="16" width="20" height="24" rx="3" class="ic-s"/><rect x="48" y="16" width="20" height="24" rx="3" class="ic-s"/><path d="M26 22h18" class="ic-a2"/><path d="M40 18l4 4-4 4" class="ic-a2"/><path d="M46 34H28" class="ic-a2"/><path d="M32 30l-4 4 4 4" class="ic-a2"/>',
    "pg": '<ellipse cx="36" cy="12" rx="20" ry="6" class="ic-s"/><path d="M16 12v30c0 3.3 9 6 20 6s20-2.7 20-6V12" class="ic-s"/><path d="M16 22c0 3.3 9 6 20 6s20-2.7 20-6M16 32c0 3.3 9 6 20 6s20-2.7 20-6" class="ic-l"/>',
    "redis": '<path d="M36 8c-9 0-14 7-14 16v10l-5 6h38l-5-6V24c0-9-5-16-14-16z" class="ic-s"/><circle cx="36" cy="46" r="4" class="ic-f"/><path d="M12 14c-3 4-3 10 0 14M60 14c3 4 3 10 0 14" class="ic-a2"/>',
    "kutu": '<path d="M8 20l10-12h36l10 12v24H8z" class="ic-s"/><path d="M8 20h18c0 5 4 8 10 8s10-3 10-8h18" class="ic-l"/><rect x="26" y="8" width="20" height="10" rx="1" class="ic-f"/>',
    "kuyruk": '<rect x="6" y="18" width="16" height="22" rx="2" class="ic-s"/><rect x="26" y="18" width="16" height="22" rx="2" class="ic-s"/><rect x="46" y="18" width="16" height="22" rx="2" class="ic-f"/><circle cx="54" cy="12" r="7" class="ic-s"/><path d="M54 8v4l3 2" class="ic-l"/>',
    "longpoll": '<path d="M22 6h28M22 50h28M26 6c0 12 20 14 20 22S26 38 26 50M46 6c0 12-20 14-20 22s20 10 20 22" class="ic-s"/><path d="M30 44h12l-6-8z" class="ic-f"/>',
    "token": '<circle cx="20" cy="28" r="12" class="ic-s"/><circle cx="20" cy="28" r="4" class="ic-f"/><path d="M32 28h32M52 28v8M60 28v6" class="ic-a2"/>',
}

PIECES = [
    ("sunucu", "Sunucu", "Bir bilgisayar. Fiziksel ya da bulutta sanal.",
     "İki tane var: <b>Sunucu 1</b> git2'nin kendisi, <b>Sunucu 2</b> işlerin koştuğu build host."),
    ("konteyner", "Konteyner", "Bir programın kendi dosyalarıyla, yalıtılmış kutusu. Docker kurar.",
     "Sunucu 1'deki her servis bir konteyner. Her CI işi de Sunucu 2'de kendi konteynerinde çalışır."),
    ("port", "Servis ve port", "Sürekli çalışan bir program. Port, ona ulaşılan kapı numarası.",
     "ci-svc'nin iki kapısı var: <code>:9090</code> iç servisler için, <code>:9091</code> runner'lar için."),
    ("rpc", "RPC (gRPC)", "Başka bir makinedeki fonksiyonu, kendi fonksiyonunu çağırır gibi çağırmak.",
     "Servisler birbirine gRPC ile konuşur. Ne gönderileceğini <code>.proto</code> dosyası belirler."),
    ("pg", "PostgreSQL", "Kalıcı veritabanı: tablolar, satırlar. Yedeklenir.",
     "Bütün CI durumu burada: pipeline'lar, işler, loglar, sırlar, kuyruk. <b>Gerçeğin tek kaynağı.</b>"),
    ("redis", "Redis", "Bellekte duran hızlı bir yardımcı. Yedeklenmez.",
     "Bizde yalnızca <b>zil</b>: &laquo;yeni iş var, uyan&raquo;. Kapalıysa her şey yine çalışır, sadece biraz yavaş."),
    ("kutu", "Outbox / Inbox", "Giden ve gelen kutusu. Olay önce tabloya yazılır, sonra gönderilir.",
     "git-svc'nin outbox'ı → ci-svc'nin inbox'ı. Hiçbir push kaybolmaz."),
    ("kuyruk", "Kuyruk ve kira", "Sırada bekleyen işler; alan, süreli bir kirayla alır.",
     "Kuyruk Postgres'teki <code>jobs</code> tablosu. Kira 60 sn ve her nabızda uzar. Uzamazsa iş geri alınır."),
    ("longpoll", "Long poll", "&laquo;Var mı?&raquo; sorusuna cevabı, bir şey olana kadar bekletmek.",
     "Runner sorar, ci-svc 25 sn'ye kadar bekletir. İş gelirse anında cevap verir."),
    ("token", "Token", "Kimlik kartı yerine geçen uzun, gizli bir metin.",
     "Kayıt için tek kullanımlık <code>g2rt_…</code>, sonra runner'ın kalıcı <code>g2r_…</code>'si. Veritabanında yalnızca özeti (SHA-256) durur."),
]

# ---------------------------------------------------------------- page
CSS = r"""
:root {
  --bg: #eef1f5; --surface: #ffffff; --surface-2: #f6f8fb; --ink: #162030; --ink-2: #4b5768;
  --line: #cfd6df; --line-2: #e3e8ee;
  --cp: #2445b8; --cp-tint: #e4eafb;
  --dp: #93500f; --dp-tint: #f8ecdf;
  --db: #0f766e; --db-tint: #dcf2ef;
  --rd: #b4234f; --rd-tint: #fbe4ec;
  --mute: #5f6b7d; --mute-tint: #eceff3;
  --ok: #1b7442; --ok-tint: #e0f1e7; --warn: #8a5a00; --warn-tint: #fbefd2; --bad: #b42318;
  --on-fill: #ffffff;
  --font-display: "IBM Plex Sans Condensed", "Arial Narrow", system-ui, sans-serif;
  --font-body: "IBM Plex Sans", system-ui, -apple-system, "Segoe UI", Roboto, Arial, sans-serif;
  --font-mono: "IBM Plex Mono", ui-monospace, "Cascadia Mono", Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: #0e131a; --surface: #151c26; --surface-2: #1a2330; --ink: #e3e8ef; --ink-2: #9ba8b9;
    --line: #2d3848; --line-2: #222b38;
    --cp: #93a9ff; --cp-tint: #1b2546; --dp: #e7a463; --dp-tint: #36230f;
    --db: #4fd1c0; --db-tint: #0f2d2a; --rd: #ff8fb0; --rd-tint: #3a1522;
    --mute: #93a0b2; --mute-tint: #202a37;
    --ok: #63cc8f; --ok-tint: #12301f; --warn: #e9b64c; --warn-tint: #33280e; --bad: #ff8d7e;
    --on-fill: #0e131a; color-scheme: dark;
  }
}
:root[data-theme="dark"] {
  --bg: #0e131a; --surface: #151c26; --surface-2: #1a2330; --ink: #e3e8ef; --ink-2: #9ba8b9;
  --line: #2d3848; --line-2: #222b38;
  --cp: #93a9ff; --cp-tint: #1b2546; --dp: #e7a463; --dp-tint: #36230f;
  --db: #4fd1c0; --db-tint: #0f2d2a; --rd: #ff8fb0; --rd-tint: #3a1522;
  --mute: #93a0b2; --mute-tint: #202a37;
  --ok: #63cc8f; --ok-tint: #12301f; --warn: #e9b64c; --warn-tint: #33280e; --bad: #ff8d7e;
  --on-fill: #0e131a; color-scheme: dark;
}
* { box-sizing: border-box; }
html { scroll-behavior: smooth; }
body { margin: 0; background: var(--bg); color: var(--ink); font: 15px/1.55 var(--font-body); }
.wrap { max-width: 1100px; margin: 0 auto; padding-block: 28px 72px;
  padding-left: max(16px, env(safe-area-inset-left, 0px)); padding-right: max(16px, env(safe-area-inset-right, 0px)); }
h1, h2, h3 { font-family: var(--font-display); text-wrap: balance; margin: 0; }
h1 { font-size: clamp(28px, 4.6vw, 42px); line-height: 1.1; }
h2 { font-size: 27px; line-height: 1.2; }
h3 { font-size: 18px; }
p { margin: 0; max-width: 72ch; }
a { color: var(--cp); }
code { font-family: var(--font-mono); font-size: .88em; background: var(--surface-2); border: 1px solid var(--line-2);
  padding: 0 4px; border-radius: 4px; overflow-wrap: anywhere; }
.eyebrow { font: 600 12px var(--font-mono); letter-spacing: .08em; color: var(--ink-2); text-transform: uppercase; }
header.top { display: grid; gap: 12px; margin-bottom: 18px; }
.lead { color: var(--ink-2); font-size: 16.5px; }
nav.toc { position: sticky; top: env(safe-area-inset-top, 0px); z-index: 5; background: var(--bg);
  border-bottom: 1px solid var(--line); padding: 8px 0; display: flex; gap: 6px; flex-wrap: wrap; }
nav.toc a { font: 600 12.5px var(--font-body); text-decoration: none; color: var(--ink); padding: 4px 10px;
  border: 1px solid var(--line); border-radius: 999px; background: var(--surface); white-space: nowrap; }
nav.toc a:hover, nav.toc a:focus-visible { border-color: var(--cp); color: var(--cp); outline: none; }
section { margin-top: 46px; display: grid; gap: 16px; scroll-margin-top: 60px; }
.sec-head { display: grid; gap: 6px; }
.sec-head .n { font: 700 13px var(--font-mono); color: var(--cp); }
.card { background: var(--surface); border: 1px solid var(--line); border-radius: 10px; padding: 16px; min-width: 0; }
.grid { display: grid; gap: 12px; }
.g2 { grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); }
.g3 { grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); }
.g5 { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); }
.piece { display: grid; grid-template-columns: 72px 1fr; gap: 12px; align-items: start; }
.piece svg { width: 72px; height: 56px; color: var(--ink); }
.piece h3 { font-size: 16px; margin-bottom: 2px; }
.piece .what { color: var(--ink-2); font-size: 13.5px; }
.piece .ours { font-size: 13.5px; margin-top: 6px; padding-top: 6px; border-top: 1px dashed var(--line); }
.ic-s { fill: none; stroke: currentColor; stroke-width: 2.2; stroke-linejoin: round; }
.ic-l { fill: none; stroke: currentColor; stroke-width: 1.4; opacity: .6; }
.ic-f { fill: var(--cp); }
.ic-a { fill: var(--ok); }
.ic-a2 { fill: none; stroke: var(--cp); stroke-width: 2.2; stroke-linecap: round; stroke-linejoin: round; }
.ic-t { font: 600 10px var(--font-mono); fill: currentColor; }
.ic-t2 { font: 600 9px var(--font-mono); fill: var(--cp); }
.fig { overflow-x: auto; margin: 0; }
.fig svg { display: block; width: 100%; height: auto; min-width: 760px; }
.fig.narrow svg { min-width: 600px; }
figcaption { color: var(--ink-2); font-size: 13.5px; margin-top: 6px; }
.legend { display: flex; flex-wrap: wrap; gap: 8px 16px; font-size: 13px; color: var(--ink-2); }
.legend span { display: inline-flex; align-items: center; gap: 6px; }
.legend i { width: 22px; height: 3px; border-radius: 2px; display: inline-block; }
/* map */
.zone { fill: none; stroke: var(--line); stroke-width: 1.4; stroke-dasharray: 6 5; }
.zl { font: 600 10.5px var(--font-body); fill: var(--ink-2); letter-spacing: .06em; }
.bx rect { fill: var(--surface-2); stroke: var(--line); stroke-width: 1.2; }
.bx .t { font: 600 13px var(--font-body); fill: var(--ink); }
.bx .s { font: 10.5px var(--font-mono); fill: var(--ink-2); }
.bx.off { opacity: .3; }
.bx.on.k-ci rect { fill: var(--cp-tint); stroke: var(--cp); stroke-width: 2.2; }
.bx.on.k-run rect { fill: var(--dp-tint); stroke: var(--dp); stroke-width: 2.2; }
.bx.on.k-db rect { fill: var(--db-tint); stroke: var(--db); stroke-width: 2.2; }
.bx.on.k-rd rect { fill: var(--rd-tint); stroke: var(--rd); stroke-width: 2.2; }
.bx.on.k-other rect { fill: var(--mute-tint); stroke: var(--mute); stroke-width: 2.2; }
.ar { fill: none; stroke-width: 2.4; stroke-linecap: round; }
.p-grpc { --c: var(--cp); } .p-sql { --c: var(--db); } .p-redis { --c: var(--rd); } .p-http { --c: var(--mute); } .p-docker { --c: var(--dp); }
.ar { stroke: var(--c); }
.ar.p-redis { stroke-dasharray: 5 4; }
.mk { fill: var(--c); }
.al { font: 600 11.5px var(--font-body); fill: var(--c); paint-order: stroke; stroke: var(--surface); stroke-width: 4px; stroke-linejoin: round; }
.badge rect { fill: var(--c); }
.badge text { font: 700 10.5px var(--font-mono); fill: var(--on-fill); }
.leg-grpc { background: var(--cp); } .leg-sql { background: var(--db); } .leg-redis { background: repeating-linear-gradient(90deg, var(--rd) 0 5px, transparent 5px 9px); }
.leg-http { background: var(--mute); } .leg-docker { background: var(--dp); }
/* steps */
.steps { display: grid; gap: 18px; counter-reset: s; }
.step { background: var(--surface); border: 1px solid var(--line); border-radius: 12px; padding: 16px; display: grid; gap: 12px; min-width: 0; }
.step-head { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.num { flex: none; width: 38px; height: 38px; border-radius: 50%; background: var(--cp); color: var(--on-fill);
  display: grid; place-items: center; font: 700 17px var(--font-mono); }
.step-head h3 { font-size: 19px; flex: 1 1 260px; }
h3 b { font-weight: inherit; color: var(--cp); }
td.port { font-family: var(--font-mono); font-size: 12.5px; white-space: normal; }
.st { font: 600 11.5px var(--font-body); padding: 2px 9px; border-radius: 999px; border: 1px solid; white-space: nowrap; }
.st-main { color: var(--ok); border-color: var(--ok); background: var(--ok-tint); }
.st-pr { color: var(--warn); border-color: var(--warn); background: var(--warn-tint); }
.st-rev { color: var(--cp); border-color: var(--cp); background: var(--cp-tint); }
.st-plan { color: var(--mute); border-color: var(--mute); background: var(--mute-tint); }
.step-body { display: grid; grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr); gap: 16px; align-items: start; }
@media (max-width: 820px) { .step-body { grid-template-columns: 1fr; } }
ol.what { margin: 0; padding-left: 20px; display: grid; gap: 6px; }
ol.what li::marker { color: var(--cp); font-weight: 700; }
.facts { display: grid; gap: 8px; align-content: start; }
.fact { display: grid; grid-template-columns: 96px 1fr; gap: 8px; font-size: 13.5px; padding: 8px 10px; border-radius: 8px; background: var(--surface-2); border: 1px solid var(--line-2); }
.fact b { font: 600 11px var(--font-body); text-transform: uppercase; letter-spacing: .05em; color: var(--ink-2); padding-top: 2px; }
.fact span { font-family: var(--font-mono); font-size: 12.5px; overflow-wrap: anywhere; }
/* tables */
.tbl { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; min-width: 640px; font-size: 14px; background: var(--surface); }
th, td { text-align: left; padding: 9px 10px; border-bottom: 1px solid var(--line-2); vertical-align: top; }
th { font: 600 11.5px var(--font-body); text-transform: uppercase; letter-spacing: .05em; color: var(--ink-2); background: var(--surface-2); }
td:first-child { font-weight: 600; white-space: nowrap; }
.kind { display: inline-block; width: 10px; height: 10px; border-radius: 3px; margin-right: 6px; vertical-align: middle; }
/* generic diagrams */
.dg text { fill: var(--ink); font: 12.5px var(--font-body); }
.dg .mono { font: 11.5px var(--font-mono); fill: var(--ink-2); }
.dg .h { font: 600 13px var(--font-body); }
.dg .small { font-size: 11px; fill: var(--ink-2); }
.dg .frame { fill: none; stroke: var(--line); stroke-width: 1.4; stroke-dasharray: 6 5; }
.dg .box { fill: var(--surface-2); stroke: var(--line); stroke-width: 1.3; }
.dg .box-ci { fill: var(--cp-tint); stroke: var(--cp); stroke-width: 2; }
.dg .box-run { fill: var(--dp-tint); stroke: var(--dp); stroke-width: 2; }
.dg .box-db { fill: var(--db-tint); stroke: var(--db); stroke-width: 2; }
.dg .box-rd { fill: var(--rd-tint); stroke: var(--rd); stroke-width: 2; }
.dg .tbl-h { fill: var(--db); }
.dg .tbl-ht { fill: var(--on-fill); font: 600 12px var(--font-mono); }
.dg .new { fill: var(--cp); font: 600 10px var(--font-body); }
.dg .ok { fill: var(--ok); } .dg .bad { fill: var(--bad); } .dg .warnc { fill: var(--warn); }
.dg .line { fill: none; stroke: var(--ink-2); stroke-width: 1.4; }
.dg .tick { stroke: var(--line); stroke-width: 1; }
.dg .bar-run { fill: var(--dp); } .dg .bar-ci { fill: var(--cp); } .dg .bar-mute { fill: var(--mute); } .dg .bar-rd { fill: var(--rd); }
.dg .n-badge { fill: var(--ink); } .dg .n-badge-t { fill: var(--surface); font: 700 11px var(--font-mono); }
.limits { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 10px; }
.limit { background: var(--surface); border: 1px solid var(--line); border-radius: 10px; padding: 12px; display: grid; gap: 4px; }
.limit .v { font: 700 24px var(--font-display); color: var(--dp); }
.limit .k { font-size: 13px; color: var(--ink-2); }
ol.steps-plain { margin: 0; padding-left: 22px; display: grid; gap: 8px; }
ol.steps-plain li::marker { font-weight: 700; color: var(--dp); }
.api { display: grid; gap: 8px; align-content: start; }
.api-h { display: flex; justify-content: space-between; gap: 8px; flex-wrap: wrap; align-items: baseline; }
.api-h h3 { font-family: var(--font-mono); font-size: 15px; }
.api ul { margin: 0; padding: 0; list-style: none; display: grid; gap: 5px; }
.api li { display: grid; grid-template-columns: auto 1fr; gap: 4px 10px; font-size: 13.5px; align-items: baseline; }
.api li code { white-space: nowrap; font-size: 12px; }
@media (max-width: 520px) { .api li { grid-template-columns: 1fr; } }
.api li code { justify-self: start; }
.who { font-size: 12.5px; color: var(--ink-2); }
.scen h3 { font-size: 16px; margin-bottom: 8px; }
.scen ol { margin: 0; padding-left: 20px; display: grid; gap: 5px; font-size: 14px; }
.scen .end { margin-top: 8px; font-size: 13px; font-weight: 600; color: var(--ok); }
.note { color: var(--ink-2); font-size: 13.5px; }
@media (prefers-reduced-motion: reduce) { html { scroll-behavior: auto; } }
"""


def legend():
    items = "".join(f'<span><i class="leg-{p}"></i>{E(t)}</span>' for p, t in PROTO.items())
    return f'<div class="legend">{items}</div>'


def section(id_, n, title, lead, body):
    return (f'<section id="{id_}"><div class="sec-head"><span class="n">{n}</span><h2>{title}</h2>'
            + (f'<p class="lead">{lead}</p>' if lead else "") + f'</div>{body}</section>')


def pieces_html():
    cards = []
    for icon, name, what, ours in PIECES:
        cards.append(f'<div class="card piece"><svg viewBox="0 0 72 56" aria-hidden="true">{ICONS[icon]}</svg>'
                     f'<div><h3>{name}</h3><p class="what">{what}</p><p class="ours">{ours}</p></div></div>')
    return f'<div class="grid g2">{"".join(cards)}</div>'


def overview_html():
    all_arrows = ["push", "gw_git", "git_pg", "deliver", "getblob", "ci_pg", "ci_rd", "run_ci", "run_dock", "gw_core", "core_ci"]
    badges = [("push", "1·12"), ("gw_git", "1"), ("git_pg", "2"), ("deliver", "3"), ("getblob", "5"),
              ("ci_pg", "4·6·8"), ("ci_rd", "4·7·10"), ("run_ci", "8·10·11", 0.45), ("run_dock", "9"),
              ("gw_core", "12"), ("core_ci", "12")]
    svg = map_svg(set(BOX), all_arrows, "Sunucu haritası: bilgisayardaki geliştirici, Sunucu 1'de gateway, git2 çekirdek, git-svc, ci-svc, PostgreSQL ve Redis; Sunucu 2'de git2-runner ve iş konteynerleri. Oklar adım numaralarını taşır.", badges)
    rows = [
        ("other", "gateway", "Sunucu 1", "443 (dışarı açık), 80", "HTTPS'i karşılar, kim olduğunu kontrol eder, isteği içeri yönlendirir", "tarayıcı, git"),
        ("other", "git2 çekirdek", "Sunucu 1", "8080 (iç)", "Web sayfaları, dash-api, admin-api, giriş. Kullanıcının yetkisine bakar", "gateway"),
        ("other", "git-svc", "Sunucu 1", "9090 gRPC · 9080 Git", "Depoları diskte tutar, dosya okutur (GetBlob), olaylarını relay ile dağıtır", "gateway, ci-svc"),
        ("ci", "ci-svc", "Sunucu 1 (Compose'a ekleme: deploy#21)", "9090 iç · 9091 runner", "Pipeline'lar, iş kuyruğu, loglar, sırlar, izleme", "git-svc relay, dash-api, admin-api, runner"),
        ("db", "PostgreSQL", "Sunucu 1", "5432 (iç)", "Kalıcı kayıt. Her servisin kendi şeması var: ci-svc yalnızca <code>ci_svc</code>'yi okur", "her servis kendi şemasına"),
        ("rd", "Redis", "Sunucu 1", "6379 (iç)", "Anlık &laquo;uyan&raquo; zilleri ve sayaçlar. Kalıcı değil, yedeklenmez", "ci-svc (ve diğer servisler)"),
        ("run", "git2-runner", "Sunucu 2", "yok (dışarı arar)", "İş ister, konteynerde koşturur, log ve sonuç gönderir", "kimse; o ci-svc:9091'i arar"),
        ("run", "Docker + iş konteynerleri", "Sunucu 2", "—", "Her işin yalıtılmış kutusu: <code>git2-job-&lt;id&gt;</code>", "runner (Docker API ile)"),
    ]
    color = {"other": "var(--mute)", "ci": "var(--cp)", "db": "var(--db)", "rd": "var(--rd)", "run": "var(--dp)"}
    trs = "".join(f'<tr><td><span class="kind" style="background:{color[k]}"></span>{n}</td><td>{w}</td><td class="port">{p}</td><td>{d}</td><td>{c}</td></tr>'
                  for k, n, w, p, d, c in rows)
    return (f'<figure class="fig"><div class="card">{svg}</div>'
            f'<figcaption>Kesik çizgili çerçeveler makineleri gösterir. Oklardaki numaralar <a href="#yolculuk">bölüm 3</a>\'teki adımlardır.</figcaption></figure>'
            + legend()
            + f'<div class="tbl"><table><thead><tr><th>Kutu</th><th>Nerede</th><th>Port</th><th>Ne yapar</th><th>Kim ona bağlanır</th></tr></thead><tbody>{trs}</tbody></table></div>'
            + '<div class="grid g3">'
              '<div class="card"><h3>Neden iki sunucu?</h3><p class="note" style="margin-top:6px">CI işleri başkalarının kodunu çalıştırır. Bir iş kötü niyetli olsa bile git2\'nin verisine, veritabanına ve sırlarına ulaşamasın diye işler ayrı bir makinede koşar.</p></div>'
              '<div class="card"><h3>Kim kimi arar?</h3><p class="note" style="margin-top:6px">Runner her zaman <b>dışarı doğru</b> ci-svc\'yi arar. Sunucu 2\'ye içeri hiçbir bağlantı açılmaz; yalnızca ci-svc\'nin 9091 portu açık olmalı.</p></div>'
              '<div class="card"><h3>Tarayıcı neyi görür?</h3><p class="note" style="margin-top:6px">Yalnızca gateway\'i. ci-svc, git-svc, Postgres ve Redis dışarıya kapalıdır; tarayıcı onlara hep dash-api ya da admin-api üzerinden ulaşır.</p></div>'
              '</div>')


def steps_html():
    out = [legend(), '<div class="steps">']
    for i, (title, on, arrows, what, facts, status) in enumerate(STEPS, 1):
        arrow_specs = []
        for a in arrows:
            name, label, lx, ly, *anc = a
            arrow_specs.append((name, label, lx, ly, anc[0] if anc else "middle"))
        svg = map_svg(set(on), arrow_specs, f"Adım {i}: " + html.unescape(title.replace("<code>", "").replace("</code>", "").replace("<b>", "").replace("</b>", "").replace("<i>", "").replace("</i>", "")))
        st_text, st_cls = STATUS[status]
        lis = "".join(f"<li>{w}</li>" for w in what)
        fs = "".join(f'<div class="fact"><b>{k}</b><span>{E(v)}</span></div>' for k, v in facts)
        out.append(f'<article class="step" id="adim-{i}"><div class="step-head"><span class="num">{i}</span><h3>{title}</h3>'
                   f'<span class="st {st_cls}">{E(st_text)}</span></div>'
                   f'<div class="fig">{svg}</div>'
                   f'<div class="step-body"><ol class="what">{lis}</ol><div class="facts">{fs}</div></div></article>')
    out.append("</div>")
    return "".join(out)


def badge(x, y, n):
    return f'<circle class="n-badge" cx="{x}" cy="{y}" r="10"/><text class="n-badge-t" x="{x}" y="{y + 4}" text-anchor="middle">{n}</text>'


def tbl(x, y, w, name, cols, new=()):
    rows = [f'<rect class="box-db" x="{x}" y="{y}" width="{w}" height="{30 + 17 * len(cols)}" rx="6"/>',
            f'<path class="tbl-h" d="M{x},{y + 24} V{y + 6} a6,6 0 0 1 6,-6 H{x + w - 6} a6,6 0 0 1 6,6 V{y + 24} Z"/>',
            f'<text class="tbl-ht" x="{x + 10}" y="{y + 16}">{E(name)}</text>']
    for i, c in enumerate(cols):
        cy = y + 24 + 17 * i + 13
        rows.append(f'<text class="mono" x="{x + 10}" y="{cy}">{E(c)}</text>')
        if c in new:
            rows.append(f'<text class="new" x="{x + w - 8}" y="{cy}" text-anchor="end">yeni · 008</text>')
    return "".join(rows)


def postgres_html():
    T = {
        "runners": (20, 20, 210, ["id", "name · labels", "token_sha256", "paused", "last_seen_at", "health"]),
        "pipelines": (262, 20, 210, ["id", "repository_id · ref", "commit_sha", "status", "idempotency_key"]),
        "jobs": (504, 20, 220, ["id", "pipeline_id", "stage · name", "status", "runner_id", "lease_expires_at", "attempt", "container_stats"]),
        "job_log_chunks": (756, 20, 160, ["job_id", "offset", "data"]),
        "secrets": (756, 136, 160, ["scope · name", "ciphertext", "nonce · key_id"]),
        "outbox": (262, 196, 210, ["event_type", "payload (JSON)", "published_at"]),
        "inbox": (504, 222, 220, ["event_id", "payload (JSON)", "processed_at", "next_attempt_at"]),
        "registration_tokens": (20, 196, 210, ["token_sha256", "expires_at", "used_at"]),
    }
    new = {"health", "container_stats"}
    parts = ['<svg viewBox="0 0 936 330" role="img" class="dg" aria-label="ci_svc şemasındaki tablolar ve aralarındaki bağlar.">']
    for name, (x, y, w, cols) in T.items():
        parts.append(tbl(x, y, w, name, cols, new))

    def row_y(name, i):
        return T[name][1] + 24 + 17 * i + 9
    # jobs.pipeline_id -> pipelines.id
    parts.append(f'<path class="line" d="M504,{row_y("jobs", 1)} H488 V{row_y("pipelines", 0)} H472" marker-end="url(#m-sql)"/>')
    # job_log_chunks.job_id -> jobs.id
    parts.append(f'<path class="line" d="M756,{row_y("job_log_chunks", 0)} H740 V{row_y("jobs", 0)} H724" marker-end="url(#m-sql)"/>')
    # jobs.runner_id -> runners.id (below pipelines)
    parts.append(f'<path class="line" d="M504,{row_y("jobs", 4)} H492 V184 H246 V{row_y("runners", 0)} H230" marker-end="url(#m-sql)"/>')
    parts.append('<text class="small" x="248" y="180">her iş, onu koşturan runner\'a bağlı</text>')
    parts.append('<text class="small" x="756" y="246">kuyruk = jobs tablosundaki</text><text class="small" x="756" y="262">pending satırlar</text>')
    parts.append("</svg>")
    er = f'<figure class="fig"><div class="card">{"".join(parts)}</div><figcaption>Oklar &laquo;bu satır şuna ait&raquo; demek. &laquo;yeni · 008&raquo; olan sütunlar izleme için eklendi (PR #52).</figcaption></figure>'

    # SKIP LOCKED race
    race = ['<svg viewBox="0 0 760 210" role="img" class="dg" aria-label="İki runner aynı anda iş ister; A ilk işi kilitler, B kilitli satırı atlayıp ikinci işi alır.">']
    race.append('<rect class="box-run" x="20" y="30" width="120" height="40" rx="7"/><text class="h" x="80" y="55" text-anchor="middle">Runner A</text>')
    race.append('<rect class="box-run" x="20" y="130" width="120" height="40" rx="7"/><text class="h" x="80" y="155" text-anchor="middle">Runner B</text>')
    race.append('<rect class="box-db" x="330" y="20" width="410" height="170" rx="8"/><text class="h" x="345" y="42">ci_svc.jobs · pending işler (en eski üstte)</text>')
    for i, (name, who) in enumerate([("build #41", "A"), ("build #42", "B"), ("lint #42", "")]):
        y = 56 + i * 42
        cls = "box-ci" if who else "box"
        race.append(f'<rect class="{cls}" x="350" y="{y}" width="370" height="32" rx="5"/>'
                    f'<text class="mono" x="364" y="{y + 21}">{E(name)}</text>')
        if who == "A":
            race.append(f'<text class="h" x="706" y="{y + 21}" text-anchor="end">kilitledi → A alır</text>')
        elif who == "B":
            race.append(f'<text class="h" x="706" y="{y + 21}" text-anchor="end">kilitli olanı atladı → B alır</text>')
        else:
            race.append(f'<text class="small" x="706" y="{y + 21}" text-anchor="end">sırada bekliyor</text>')
    race.append('<path class="line" d="M140,50 L346,72" marker-end="url(#m-grpc)"/><text class="small" x="230" y="52">aynı anda sorar</text>')
    race.append('<path class="line" d="M140,150 C240,150 260,90 346,114" marker-end="url(#m-grpc)"/><text class="small" x="196" y="182">aynı anda sorar</text>')
    race.append("</svg>")
    race_fig = f'<figure class="fig narrow"><div class="card">{"".join(race)}</div><figcaption><code>FOR UPDATE SKIP LOCKED</code>: bir satırı alan onu kilitler, aynı anda soran başkası kilitliyi atlar ve sıradakini alır. Bu yüzden iki runner aynı işi asla koşturmaz.</figcaption></figure>'

    facts = ('<div class="grid g3">'
             '<div class="card"><h3>Neden kuyruk Postgres\'te?</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>İş, pipeline ile aynı işlemde yazılır: ikisi asla birbirinden kopmaz.</li>'
             '<li>Postgres yedeklenir. Geri yüklemede kuyruk da geri gelir.</li>'
             '<li>Redis çökse bile hiçbir iş kaybolmaz (NFR-21).</li></ol></div>'
             '<div class="card"><h3>Hepsi ya da hiçbiri</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>Pipeline oluşurken birkaç tablo birlikte yazılır.</li>'
             '<li>Biri bile başarısız olursa hiçbiri yazılmaz (transaction).</li>'
             '<li>Olay (outbox satırı) da aynı işlemde yazılır: olay varsa değişiklik de vardır.</li></ol></div>'
             '<div class="card"><h3>Sırlar nasıl saklanır?</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>Değer AES-256-GCM ile şifrelenir. Tabloda yalnızca şifreli hali durur.</li>'
             '<li>Anahtar (<code>GIT2_CI_SECRETS_KEY</code>) veritabanında değil, sunucunun ayarlarında durur.</li>'
             '<li>Bir kez kaydedilen sır API\'den bir daha okunamaz; yalnızca o iş koşarken runner\'a gider.</li></ol></div>'
             '</div>')
    return er + race_fig + facts


def redis_html():
    s = ['<svg viewBox="0 0 860 230" role="img" class="dg" aria-label="ci-svc kopya 1 zili Redis\'e yayınlar, Redis kopya 2\'ye iletir, kopya 2\'de bekleyen runner isteği uyanır.">']
    s.append('<rect class="box-ci" x="20" y="40" width="170" height="56" rx="8"/><text class="h" x="105" y="64" text-anchor="middle">ci-svc · kopya 1</text><text class="small" x="105" y="82" text-anchor="middle">yeni iş kuyruğa girdi</text>')
    s.append('<rect class="box-rd" x="340" y="40" width="180" height="56" rx="8"/><text class="h" x="430" y="64" text-anchor="middle">Redis</text><text class="mono" x="430" y="82" text-anchor="middle">ci:jobs:wake</text>')
    s.append('<rect class="box-ci" x="670" y="40" width="170" height="56" rx="8"/><text class="h" x="755" y="64" text-anchor="middle">ci-svc · kopya 2</text><text class="small" x="755" y="82" text-anchor="middle">runner bekletiliyor</text>')
    s.append('<rect class="box-run" x="670" y="150" width="170" height="44" rx="8"/><text class="h" x="755" y="177" text-anchor="middle">Runner X</text>')
    s.append('<path class="ar p-redis" d="M190,68 H334" marker-end="url(#m-redis)"/><text class="al p-redis" x="262" y="60" text-anchor="middle">1 · yayınla</text>')
    s.append('<path class="ar p-redis" d="M520,68 H664" marker-end="url(#m-redis)"/><text class="al p-redis" x="592" y="60" text-anchor="middle">2 · dağıt</text>')
    s.append('<path class="ar p-grpc" d="M755,96 V144" marker-end="url(#m-grpc)"/><text class="al p-grpc" x="746" y="125" text-anchor="end">3 · &quot;iş var&quot; cevabı</text>')
    s.append('<text class="small" x="20" y="130">Zil veri taşımaz. Uyanan taraf neyin değiştiğini Postgres\'ten okur.</text>')
    s.append('<text class="small" x="20" y="150">Runner Redis\'e hiç bağlanmaz; zili duyan, onun adına bekleyen ci-svc\'dir.</text>')
    s.append("</svg>")
    fig = f'<figure class="fig"><div class="card">{"".join(s)}</div></figure>'
    rows = [
        ("<code>ci:jobs:wake</code>", "Yeni iş kuyruğa girdi", "Bekleyen RequestJob'lar", "3 sn'de bir Postgres'e bakılır"),
        ("<code>ci:inbox:wake</code>", "Yeni push olayı geldi", "Inbox işçisi", "5 sn'de bir bakılır"),
        ("<code>ci:log:&lt;job_id&gt;</code>", "İşin logu büyüdü ya da iş bitti", "Canlı log izleyenler (StreamJobLog)", "1 sn'de bir bakılır"),
        ("<code>ci:rl:register:&lt;ip&gt;</code>", "Runner kayıt denemesi", "Hız sınırı: dakikada 10", "Sınır uygulanmaz"),
    ]
    trs = "".join(f"<tr><td>{a}</td><td>{b}</td><td>{c}</td><td>{d}</td></tr>" for a, b, c, d in rows)
    table = f'<div class="tbl"><table><thead><tr><th>Anahtar / kanal</th><th>Ne zaman çalar</th><th>Kim duyar</th><th>Redis yoksa</th></tr></thead><tbody>{trs}</tbody></table></div>'

    # timeline with / without Redis
    t = ['<svg viewBox="0 0 860 150" role="img" class="dg" aria-label="Redis varken iş milisaniyeler içinde verilir; Redis yokken en çok 3 saniye sonra.">']
    x0, x1 = 170, 820
    for i in range(4):
        x = x0 + (x1 - x0) * i / 3
        t.append(f'<line class="tick" x1="{x}" y1="20" x2="{x}" y2="122"/><text class="small" x="{x}" y="140" text-anchor="middle">{i} sn</text>')
    t.append('<text class="h" x="20" y="52">Redis varken</text><rect class="bar-ci" x="170" y="38" width="6" height="22" rx="2"/><text class="small" x="184" y="54">iş hemen verilir (ms)</text>')
    t.append('<text class="h" x="20" y="100">Redis yokken</text><rect class="bar-mute" x="170" y="86" width="650" height="22" rx="3" opacity=".35"/><rect class="bar-ci" x="814" y="86" width="6" height="22" rx="2"/><text class="small" x="804" y="102" text-anchor="end">en geç 3 sn sonra Postgres yoklamasıyla</text>')
    t.append("</svg>")
    timeline = f'<figure class="fig narrow"><div class="card">{"".join(t)}</div><figcaption>Redis yalnızca hız kazandırır; doğruluk hiçbir zaman ona bağlı değildir.</figcaption></figure>'
    return fig + table + timeline


def rpc_html():
    s = ['<svg viewBox="0 0 860 250" role="img" class="dg" aria-label="dash-api ci-svc\'nin GetPipeline fonksiyonunu çağırır: istek mesajı gider, cevap mesajı döner; ikisi de pipeline.proto sözleşmesinden üretilmiş koddur.">']
    s.append('<rect class="box" x="20" y="60" width="200" height="110" rx="9"/><text class="h" x="120" y="88" text-anchor="middle">Arayan: dash-api</text>')
    s.append('<text class="mono" x="120" y="114" text-anchor="middle">p := ci.GetPipeline(id)</text><text class="small" x="120" y="138" text-anchor="middle">kendi fonksiyonu gibi çağırır</text>')
    s.append('<rect class="box-ci" x="640" y="60" width="200" height="110" rx="9"/><text class="h" x="740" y="88" text-anchor="middle">Sunucu: ci-svc :9090</text>')
    s.append('<text class="small" x="740" y="114" text-anchor="middle">Postgres\'ten okur,</text><text class="small" x="740" y="132" text-anchor="middle">cevabı doldurur</text>')
    s.append('<path class="ar p-grpc" d="M220,90 H634" marker-end="url(#m-grpc)"/>')
    s.append('<rect class="box" x="300" y="66" width="260" height="22" rx="4"/><text class="mono" x="430" y="81" text-anchor="middle">GetPipelineRequest { id: "01a1…" }</text>')
    s.append('<path class="ar p-grpc" d="M640,146 H226" marker-end="url(#m-grpc)"/>')
    s.append('<rect class="box" x="270" y="152" width="320" height="22" rx="4"/><text class="mono" x="430" y="167" text-anchor="middle">GetPipelineResponse { status: RUNNING, … }</text>')
    s.append('<rect class="box-db" x="330" y="196" width="200" height="40" rx="6"/><text class="mono" x="430" y="221" text-anchor="middle">pipeline.proto (sözleşme)</text>')
    s.append('<path class="line" d="M330,216 C200,216 120,200 120,172"/><path class="line" d="M530,216 C660,216 740,200 740,172"/>')
    s.append('<text class="small" x="170" y="236" text-anchor="middle">üretilmiş istemci kodu</text><text class="small" x="690" y="236" text-anchor="middle">üretilmiş sunucu kodu</text>')
    s.append('<text class="small" x="430" y="40" text-anchor="middle">HTTP/2 üzerinde ikili (binary) mesajlar · hata olursa kod: NOT_FOUND, INVALID_ARGUMENT, UNAVAILABLE…</text>')
    s.append("</svg>")
    fig = f'<figure class="fig"><div class="card">{"".join(s)}</div></figure>'
    howto = ('<div class="card"><h3>Bir RPC adım adım</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>Sözleşme yazılır: <code>.proto</code> dosyasında fonksiyon adı, isteğin ve cevabın alanları (<code>app/proto/ci/v1/</code>).</li>'
             '<li><code>buf generate</code> iki tarafın Go kodunu üretir. Elle mesaj ayrıştırmak yok.</li>'
             '<li>Arayan, üretilmiş istemciyle fonksiyonu çağırır; istek HTTP/2 üzerinden ikili olarak gider.</li>'
             '<li>Sunucu işi yapar ve cevabı ya da standart bir hata kodunu döndürür.</li>'
             '<li>Sözleşmeye yeni alan eklemek eski programları bozmaz; bilmedikleri alanı görmezden gelirler.</li></ol></div>')
    apis = [
        ("PipelineService", "dash-api → ci-svc :9090", "st-main", "main'de", [
            ("TriggerPipeline", "elle pipeline başlat (&laquo;Run&raquo; düğmesi)"),
            ("GetPipeline", "bir pipeline'ı ve işlerini göster"),
            ("ListPipelines", "deponun pipeline listesi"),
            ("CancelPipeline", "durdur"),
            ("RetryJob", "bir işi yeniden koştur (yeni deneme)"),
            ("GetJob", "tek işin ayrıntısı"),
            ("StreamJobLog", "canlı log akışı"),
            ("ValidateConfig", "<code>.git2-ci.yml</code> doğru mu?")]),
        ("RunnerAdminService", "admin-api → ci-svc :9090", "st-main", "main'de", [
            ("CreateRegistrationToken", "yeni runner için tek kullanımlık token"),
            ("ListRunners", "runner listesi, durumu, sağlığı"),
            ("GetRunner", "tek runner"),
            ("UpdateRunner", "duraklat / not ekle"),
            ("DeleteRunner", "sil (token hemen geçersiz)"),
            ("ListRunningJobs", "şu an ne nerede koşuyor (#52)"),
            ("GetOverview", "panodaki sayılar (#52)")]),
        ("SecretService", "admin-api, dash-api → ci-svc :9090", "st-main", "main'de", [
            ("SetSecret", "sır yaz (şifrelenir)"),
            ("ListSecrets", "adları listele (değer asla dönmez)"),
            ("DeleteSecret", "sil")]),
        ("RunnerService", "runner → ci-svc :9091 (token ile)", "st-main", "main'de", [
            ("RegisterRunner", "kayıt token'ını runner token'ıyla değiş"),
            ("RequestJob", "iş var mı? (25 sn bekletir) + sağlık raporu"),
            ("UpdateJob", "nabız (15 sn) ya da sonuç + konteyner ölçümü"),
            ("AppendLog", "log parçası ekle")]),
        ("EventSink", "git-svc relay → ci-svc :9090", "st-pr", "PR #49", [
            ("Deliver", "bir olayı teslim et (inbox'a yazılır)")]),
        ("CommitService (git-svc'nin)", "ci-svc → git-svc :9090", "st-main", "main'de", [
            ("GetBlob", "commit'teki <code>.git2-ci.yml</code>'yi oku"),
            ("GetArchive", "commit'in dosyalarını tar olarak al (#19 ile)")]),
    ]
    cards = []
    for name, who, st, stt, rpcs in apis:
        lis = "".join(f"<li><code>{E(r)}</code><span>{d}</span></li>" for r, d in rpcs)
        cards.append(f'<div class="card api"><div class="api-h"><h3>{E(name)}</h3><span class="st {st}">{E(stt)}</span></div>'
                     f'<p class="who">{E(who)}</p><ul>{lis}</ul></div>')
    events = ('<div class="card"><h3>Olaylar (ci-svc\'nin outbox\'ı)</h3><p class="note" style="margin-top:6px">'
              '<code>PipelineCreated</code>, <code>JobFinished</code>, <code>PipelineFinished</code>: ci-svc bunları <code>ci_svc.outbox</code>\'a yazar. '
              'dash-api\'ye taşıyacak relay henüz yok (#48); o zamana kadar kutuda bekler, kaybolmaz.</p></div>')
    return fig + howto + f'<div class="grid g2">{"".join(cards)}</div>' + events


def container_html():
    s = ['<svg viewBox="0 0 860 330" role="img" class="dg" aria-label="Sunucu 2: git2-runner süreci Docker Engine API ile image, volume, ağ ve konteyneri sırayla kurar; konteyner /workspace olarak volume\'u görür.">']
    s.append('<rect class="frame" x="8" y="10" width="844" height="312" rx="12"/><text class="small" x="20" y="312">SUNUCU 2 · build host</text>')
    s.append('<rect class="box-run" x="30" y="120" width="160" height="70" rx="9"/><text class="h" x="110" y="148" text-anchor="middle">git2-runner süreci</text><text class="small" x="110" y="168" text-anchor="middle">kendi config.json\'u</text>')
    s.append('<rect class="frame" x="260" y="30" width="572" height="270" rx="10" style="stroke:var(--dp);stroke-dasharray:none"/><text class="h" x="276" y="54">Docker Engine (daemon)</text>')
    s.append('<path class="ar p-docker" d="M190,155 H254" marker-end="url(#m-docker)"/><text class="al p-docker" x="222" y="146" text-anchor="middle">API</text>')
    s.append('<text class="small" x="30" y="214">DOCKER_HOST ya da yerel soket</text>')
    # image
    s.append('<rect class="box" x="290" y="76" width="150" height="54" rx="7"/><text class="h" x="365" y="99" text-anchor="middle">image</text><text class="mono" x="365" y="118" text-anchor="middle">alpine:3.20</text>')
    s.append(badge(290, 76, 1))
    # volume
    s.append('<rect class="box" x="290" y="170" width="150" height="54" rx="7"/><text class="h" x="365" y="193" text-anchor="middle">volume</text><text class="mono" x="365" y="212" text-anchor="middle">git2-job-&lt;id&gt;</text>')
    s.append(badge(290, 170, 2))
    # network
    s.append('<rect class="box" x="470" y="246" width="340" height="40" rx="7"/><text class="h" x="490" y="271">ağ (bridge)</text><text class="mono" x="800" y="271" text-anchor="end">git2-job-&lt;id&gt;</text>')
    s.append(badge(470, 246, 3))
    # container
    s.append('<rect class="box-run" x="500" y="76" width="310" height="148" rx="9"/><text class="h" x="516" y="99">konteyner git2-job-&lt;id&gt;</text>')
    s.append('<text class="mono" x="516" y="122">/bin/sh -c "set -e; …script…"</text>')
    s.append('<text class="mono" x="516" y="142">ortam: değişkenler + sırlar</text>')
    s.append('<text class="mono" x="516" y="162">/workspace  ← volume</text>')
    s.append('<text class="mono" x="516" y="182">2 GB · 2 CPU · 1024 süreç</text>')
    s.append('<text class="mono" x="516" y="202">etiket: job-id, runner-id</text>')
    s.append(badge(500, 76, 4))
    s.append('<path class="line" d="M440,197 H494" marker-end="url(#m-docker)"/>')
    s.append('<path class="line" d="M440,103 H494" marker-end="url(#m-docker)"/>')
    s.append('<path class="line" d="M655,224 V240" marker-end="url(#m-docker)"/>')
    s.append("</svg>")
    fig = f'<figure class="fig"><div class="card">{"".join(s)}</div><figcaption>Çalışma ortamı (runtime) Docker Engine. Runner <code>docker</code> komutunu değil, Docker\'ın Go API\'sini kullanır (ADR 0008).</figcaption></figure>'
    steps = [
        ("Image'a bak", "<code>ImageInspect</code>. Yoksa <code>ImagePull</code> ile çekilir. Çekilemezse iş <b>IMAGE_PULL_FAILURE</b> olur."),
        ("Volume kur", "<code>VolumeCreate git2-job-&lt;id&gt;</code>. İşin dosyaları burada durur; konteynerde <code>/workspace</code> olarak görünür."),
        ("Ağ kur", "<code>NetworkCreate</code> (bridge). Her iş kendi ağında; başka işlerin konteynerlerini göremez."),
        ("Konteyneri oluştur", "<code>ContainerCreate</code>: image, <code>/bin/sh -c</code> + script, ortam değişkenleri (sırlar dahil, diske yazılmaz), sınırlar, iki etiket."),
        ("Çıktıya bağlan", "<code>ContainerAttach</code>: stdout ve stderr runner'ın log gönderen parçasına akar, sırlar orada maskelenir."),
        ("Bitişi beklemeye başla", "<code>ContainerWait</code> başlatmadan <i>önce</i> çağrılır; çok hızlı biten bir iş gözden kaçmasın."),
        ("Başlat", "<code>ContainerStart</code>. Script çalışır, her satır logda görünür."),
        ("Sonucu al", "Çıkış kodu 0 ise SUCCESS, değilse FAILED. Süre dolarsa konteyner öldürülür ve iş TIMEOUT olur."),
        ("Temizle", "Ne olursa olsun önce konteyner (zorla), sonra ağ, sonra volume silinir. Geride bir şey kalmaz."),
    ]
    lis = "".join(f"<li><b>{a}.</b> {b}</li>" for a, b in steps)
    plan = f'<div class="card"><h3>Bir işin konteyneri, adım adım</h3><ol class="steps-plain" style="margin-top:10px">{lis}</ol></div>'
    limits = ('<div class="limits">'
              '<div class="limit"><span class="v">2048 MB</span><span class="k">bellek sınırı; aşan iş öldürülür</span></div>'
              '<div class="limit"><span class="v">2 CPU</span><span class="k">işlemci sınırı</span></div>'
              '<div class="limit"><span class="v">1024</span><span class="k">süreç ve iş parçacığı sınırı</span></div>'
              '<div class="limit"><span class="v">1 sa</span><span class="k">varsayılan süre sınırı (en çok 24 sa, yml\'de <code>timeout</code>)</span></div>'
              '<div class="limit"><span class="v">ayrıcalıksız</span><span class="k"><code>no-new-privileges</code>, privileged değil</span></div>'
              '<div class="limit"><span class="v">soketsiz</span><span class="k">Docker soketini ve host klasörlerini göremez</span></div>'
              '</div><p class="note">İlk üç sınır runner\'ın <code>config.json</code> dosyasındaki <code>docker</code> bölümünden değiştirilebilir.</p>')
    crash = ('<div class="card"><h3>Runner çökerse konteynerler ne olur? <span class="st st-pr">PR #50 · onaylı, merge bekliyor</span></h3><ol class="steps-plain" style="margin-top:10px">'
             '<li>Runner ya da makine iş ortasında ölür. Konteyner, ağ ve volume Docker\'da kalır, konteyner çalışmaya devam edebilir.</li>'
             '<li>ci-svc\'de işin kirası 60 sn içinde dolar; temizlikçi (her 15 sn) işi <b>RUNNER_LOST</b> olarak kapatır. Kullanıcı <code>RetryJob</code> ile yeniden koşturabilir.</li>'
             '<li>Runner yeniden başlarken önce Docker\'da <code>io.git2.runner-id=&lt;kendi id&gt;</code> etiketli her şeyi bulur.</li>'
             '<li>Bunları siler ve adlarını loglar. <b>Başka runner\'ların işlerine dokunmaz</b>, çünkü onların etiketi farklıdır.</li></ol></div>')
    return fig + plan + limits + crash


def runner_life_html():
    s = ['<svg viewBox="0 0 860 210" role="img" class="dg" aria-label="Runner\'ın zaman çizelgesi: bir kez kayıt, açılışta temizlik, iş isteme döngüsü, iş sırasında 15 saniyede bir nabız ve sürekli log, sonunda sonuç.">']
    s.append('<line class="tick" x1="20" y1="120" x2="840" y2="120"/>')
    segs = [
        (20, 110, "bar-mute", "kayıt", "bir kez"),
        (110, 170, "bar-run", "temizlik", "açılışta"),
        (170, 340, "bar-ci", "RequestJob", "≤ 25 sn bekler"),
        (340, 720, "bar-run", "iş koşuyor", ""),
        (720, 800, "bar-ci", "sonuç", "UpdateJob"),
        (800, 840, "bar-ci", "", ""),
    ]
    for x0, x1, cls, label, sub in segs:
        s.append(f'<rect class="{cls}" x="{x0}" y="108" width="{x1 - x0 - 4}" height="24" rx="4"/>')
        if label:
            s.append(f'<text class="h" x="{(x0 + x1) / 2}" y="98" text-anchor="middle">{E(label)}</text>')
        if sub:
            s.append(f'<text class="small" x="{(x0 + x1) / 2}" y="152" text-anchor="middle">{E(sub)}</text>')
    for x in range(400, 720, 80):
        s.append(f'<line x1="{x}" y1="104" x2="{x}" y2="136" style="stroke:var(--cp);stroke-width:3"/>')
    s.append('<text class="small" x="530" y="152" text-anchor="middle">mavi çizgiler: 15 sn\'de bir nabız (+ CPU/bellek ölçümü)</text>')
    s.append('<text class="small" x="530" y="170" text-anchor="middle">arada sürekli: AppendLog (yarım saniyelik parçalar)</text>')
    s.append('<path class="line" d="M830,106 C830,60 340,60 335,104" marker-end="url(#m-grpc)"/>')
    s.append('<text class="small" x="540" y="58" text-anchor="middle">bitince yeniden &laquo;iş var mı?&raquo;</text>')
    s.append("</svg>")
    fig = f'<figure class="fig"><div class="card">{"".join(s)}</div></figure>'
    steps = ('<div class="grid g2">'
             '<div class="card"><h3>Kayıt (bir kez)</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>Yönetici konsolda &laquo;Yeni runner&raquo;a basar. ci-svc tek kullanımlık bir <code>g2rt_…</code> token üretir; 1 saat geçerlidir.</li>'
             '<li>Build host\'ta <code>git2-runner register -url https://…:9091 -token g2rt_…</code> çalıştırılır.</li>'
             '<li>ci-svc kayıt token\'ını yakar ve kalıcı bir <code>g2r_…</code> runner token\'ı verir.</li>'
             '<li>Runner bunu <code>config.json</code>\'a yalnızca kendisinin okuyabileceği izinle (0600) yazar.</li></ol></div>'
             '<div class="card"><h3>Çalışma (<code>git2-runner run</code>)</h3><ol class="steps-plain" style="margin-top:8px">'
             '<li>Docker\'a ulaşabildiğini kontrol eder, önceki çökmeden kalanları siler.</li>'
             '<li>&laquo;İş var mı?&raquo; diye sorar, sağlık raporunu da ekler (Docker sürümü, CPU, bellek).</li>'
             '<li>İş gelince koşturur; nabız ve logu ayrı ayrı gönderir.</li>'
             '<li>Sonucu gönderir, geri döner. <b>Aynı anda tek iş</b> koşturur.</li>'
             '<li>SIGTERM gelirse elindeki işi bitirir, sonra kapanır.</li></ol></div>'
             '</div>')
    return fig + steps


def scenarios_html():
    sc = [
        ("Runner ya da build host ölürse", [
            "Nabızlar kesilir, kira uzamaz.",
            "En geç 60 sn sonra kira dolar; temizlikçi her 15 sn'de bir bakar.",
            "İş <b>RUNNER_LOST</b> olur, pipeline devam eder ya da biter.",
            "Kullanıcı <code>RetryJob</code> ile işi yeniden koşturur. Runner geri gelince eski konteynerleri siler."],
         "Kayıp: o işin o denemesi. Pipeline kilitlenmez."),
        ("git-svc kapalıysa", [
            "Olay ci-svc'nin inbox'ında bekler.",
            "İşçi 5 sn, 10 sn, 20 sn… en çok 5 dk arayla tekrar dener.",
            "git-svc açılınca pipeline oluşur."],
         "Kayıp: yok. Yalnızca gecikme."),
        ("Redis kapalıysa", [
            "Zil çalmaz.",
            "Bekleyen istekler 3 sn'de bir, log izleyenler 1 sn'de bir, inbox işçisi 5 sn'de bir Postgres'e bakar.",
            "ci-svc &laquo;hazır&raquo; olmaya devam eder."],
         "Kayıp: yok. Birkaç saniye gecikme."),
        ("ci-svc yeniden başlarsa", [
            "Bütün durum Postgres'te, bellekte değil.",
            "Bekleyen long poll'lar kesilir; runner'lar kısa bir beklemeden sonra tekrar sorar.",
            "Koşan işlerin nabzı kira dolmadan dönerse iş kesintisiz sürer."],
         "Kayıp: yok."),
        ("Aynı olay iki kez gelirse", [
            "Relay en az bir kez teslim eder, bazen iki kez.",
            "<code>event_id</code> inbox'ta benzersiz: ikinci kayıt yazılmaz.",
            "Yine de işlenirse pipeline anahtarı <code>&lt;event_id&gt;:&lt;ref&gt;</code> ikinci pipeline'ı engeller."],
         "Çift pipeline: olmaz."),
        ("Sunucu yedekten geri yüklenirse", [
            "Postgres yedeği, şifreleme anahtarı ve depolar geri gelir.",
            "Yedekte &laquo;koşuyor&raquo; görünen işlerin kirası dolar ve RUNNER_LOST olur.",
            "Gönderilmemiş olaylar tekrar gönderilir; çiftler yukarıdaki gibi elenir."],
         "Kayıp: yedekten sonra olanlar."),
    ]
    cards = []
    for title, steps, end in sc:
        lis = "".join(f"<li>{x}</li>" for x in steps)
        cards.append(f'<div class="card scen"><h3>{title}</h3><ol>{lis}</ol><p class="end">{end}</p></div>')
    return f'<div class="grid g3">{"".join(cards)}</div>'


def modes_html():
    rows = [
        ("ci-svc", "Normal", "<code>GIT_SVC_GRPC_ADDR</code> verilir; <code>.git2-ci.yml</code> git-svc'den okunur"),
        ("ci-svc", "Geliştirme (<code>GIT2_CISVC_DEV=true</code>)", "grpcurl için reflection açılır; <code>.git2-ci.yml</code> yerel bir klasörden okunabilir (<code>GIT2_CISVC_DEV_CONFIG_DIR</code>)"),
        ("ci-svc", "Redis'siz", "Çalışır; tek kopya için uygun. Zil yerine yoklama"),
        ("ci-svc", "Supabase havuzu (port 6543)", "Ortak geliştirme veritabanı. Hazırlanmış SQL ifadesi kullanılmaz, kod iki modda da test edilir"),
        ("ci-svc", "Runner portunda TLS", "<code>GIT2_CISVC_RUNNER_TLS_CERT/_KEY</code> ya da önünde TLS sonlandıran bir proxy"),
        ("ci-svc", "Birden çok kopya", "Durum Postgres'te, zil Redis'te: kopyalar birbirini bilmeden çalışır"),
        ("runner", "systemd servisi", "<code>git2-runner</code> kullanıcısı + docker grubu; <code>deploy/systemd/git2-runner.service</code>"),
        ("runner", "Konteynerde", "<code>ghcr.io/git2-project/git2-runner</code> imajı, host'un Docker soketiyle"),
        ("runner", "Docker-in-Docker", "<code>DOCKER_HOST</code> ayrı bir Docker daemon'unu gösterir; geliştirme ortamı böyle"),
        ("runner", "Etiketler", "Runner'ın etiketleri işin <code>tags</code> listesinin hepsini içermeli; etiketsiz iş her runner'a gidebilir"),
    ]
    trs = "".join(f"<tr><td>{a}</td><td>{b}</td><td>{c}</td></tr>" for a, b, c in rows)
    return f'<div class="tbl"><table><thead><tr><th>Program</th><th>Mod</th><th>Ne değişir</th></tr></thead><tbody>{trs}</tbody></table></div>'


def roadmap_html():
    items = [
        ("st-pr", "onaylı · merge bekliyor", "Push → pipeline", "PR #49 (adım 2–5) ve çöken runner temizliği PR #50. Senin merge tıklamanı bekliyor."),
        ("st-rev", "incelemede", "İzleme", "PR #52: runner sağlığı, şu an koşan işler, pano sayıları. Migration 008: git2 #53 (Ceyda). Konsol ekranları: git2 #54 (Beste)."),
        ("st-plan", "sıradaki", "İş kaynağı (#19)", "Bugün işler boş bir <code>/workspace</code> ile başlıyor. <code>GetArchive</code> ile commit'in dosyaları konteynere kopyalanacak (codeplane v0.4.0 hazır)."),
        ("st-plan", "planlanan", "Kendi relay'imiz (#48)", "ci-svc'nin olayları dash-api'ye gidecek: merge isteğindeki ✓ / ✗ için (M3, 11 Ara)."),
        ("st-plan", "planlanan", "Artifact'lar (#21)", "İşin ürettiği dosyalar ve tamamlanan loglar bucket'ta (<code>ci/</code> öneki)."),
        ("st-plan", "planlanan", "Hata ve geri yükleme testleri (#22)", "M4 (18 Ara): iş ortasında Redis'i, ci-svc'yi ve runner'ı öldürüp sistemin toparlandığını göstermek."),
    ]
    cards = "".join(f'<div class="card"><span class="st {c}">{E(s)}</span><h3 style="margin-top:8px">{t}</h3><p class="note" style="margin-top:6px">{d}</p></div>'
                    for c, s, t, d in items)
    return f'<div class="grid g3">{cards}</div>'


def page():
    toc = [("parcalar", "1 · Parçalar"), ("sunucular", "2 · Sunucular"), ("yolculuk", "3 · Push'un yolculuğu"),
           ("postgres", "4 · Postgres"), ("redis", "5 · Redis"), ("konteyner", "6 · Konteyner"),
           ("runner", "7 · Runner"), ("rpc", "8 · RPC ve API"), ("bozulursa", "9 · Bozulursa"),
           ("modlar", "10 · Modlar"), ("yolda", "11 · Sırada")]
    nav = "".join(f'<a href="#{i}">{E(t)}</a>' for i, t in toc)
    body = [
        '<header class="top"><span class="eyebrow">git2 · CI şeridi · 20 Kasım 2026</span>'
        '<h1>Bir <code style="font-size:.8em">git push</code>\'tan yeşil ✓\'ye: git2 CI adım adım</h1>'
        '<p class="lead">Önce parçaları tanıyoruz, sonra sunucuları çiziyoruz, sonra bir push\'u 12 adımda baştan sona izliyoruz. '
        'Her adımda aynı harita çizili; o adımda çalışan kutular ve oklar yanıyor.</p></header>',
        f'<nav class="toc" aria-label="Bölümler">{nav}</nav>',
        section("parcalar", "1", "Önce parçalar", "Sayfada geçen on kavram. Her kartın altında &laquo;bizde ne yapar&raquo; yazıyor.", pieces_html()),
        section("sunucular", "2", "Sunucular ve kutular", "Hangi program hangi makinede, hangi kapıdan konuşuyor.", overview_html()),
        section("yolculuk", "3", "Bir push'un yolculuğu: 12 adım", "Geliştirici <code>main</code>'e bir commit itiyor. <code>.git2-ci.yml</code>'de <code>build</code> ve <code>test</code> diye iki aşama var.", steps_html()),
        section("postgres", "4", "Postgres'in içinde ne var?", "<code>ci_svc</code> şeması: ci-svc'nin bütün hafızası. Başka servislerin şemalarına dokunmaz.", postgres_html()),
        section("redis", "5", "Redis ne işe yarar?", "Bir kapı zili gibi: &laquo;bir şey oldu, bak&raquo; der. Neyin olduğunu söylemez, saklamaz.", redis_html()),
        section("konteyner", "6", "Konteyner nasıl oluşuyor?", "Sunucu 2'nin içine yakından bakış: runner, Docker Engine ve bir işin kutusu.", container_html()),
        section("runner", "7", "Runner'ın hayatı", "Kayıttan iş bitişine kadar bir runner'ın zaman çizelgesi.", runner_life_html()),
        section("rpc", "8", "RPC nedir, hangi RPC'ler var?", "Servisler birbirine &laquo;fonksiyon çağrısı&raquo; gönderir. Hangi fonksiyonlar var, kim çağırıyor?", rpc_html()),
        section("bozulursa", "9", "Bir şey bozulursa", "Her senaryo adım adım. Ortak kural: kalıcı olan her şey Postgres'te.", scenarios_html()),
        section("modlar", "10", "Çalışma modları", None, modes_html()),
        section("yolda", "11", "Nerede kaldık, sırada ne var?", None, roadmap_html()),
    ]
    return ("<!doctype html>\n<html lang=\"tr\"><head><meta charset=\"utf-8\">"
            "<meta name=\"viewport\" content=\"width=device-width, initial-scale=1, viewport-fit=cover\">"
            "<title>git2 CI Adım Adım</title>"
            "<link rel=\"preconnect\" href=\"https://fonts.googleapis.com\">"
            "<link rel=\"stylesheet\" href=\"https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;600;700&family=IBM+Plex+Sans+Condensed:wght@600;700&family=IBM+Plex+Sans:wght@400;600;700&display=swap\">"
            f"<style>{CSS}</style></head><body>"
            f'<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs>{MARKERS}</defs></svg>'
            f'<div class="wrap">{"".join(body)}</div></body></html>\n')


if __name__ == "__main__":
    with open(sys.argv[1], "w", encoding="utf-8", newline="\n") as f:
        f.write(page())
    print("written", sys.argv[1])
