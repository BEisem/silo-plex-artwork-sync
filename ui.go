package main

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// The status page copies Silo's "Midnight Cinema" theme (web/src/app.css):
// the same color tokens, radius and Plus Jakarta Sans, which Silo self-hosts
// and so does this plugin (SIL OFL, see assets/OFL-PlusJakartaSans.txt).
// Plugin pages are served from Silo's own origin, so the page reads Silo's
// branding document and applies the admin accent color the same way the Silo
// web app does (web/src/lib/accentColor.ts).

//go:embed assets/*.woff2 assets/OFL-PlusJakartaSans.txt
var assetFS embed.FS

func serveAsset(name string) *pluginv1.HandleHTTPResponse {
	name = path.Base(name)
	data, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		return textResponse(http.StatusNotFound, "not found")
	}
	ct := "text/plain; charset=utf-8"
	if strings.HasSuffix(name, ".woff2") {
		ct = "font/woff2"
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": ct, "Cache-Control": "public, max-age=31536000, immutable"},
		Body:       data,
	}
}

const pageHead = `<!doctype html>
<html lang="en" data-theme="midnight-cinema"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Plex Artwork Sync</title>
<style>
@font-face{font-family:"Plus Jakarta Sans";font-style:normal;font-display:swap;font-weight:200 800;
  src:url("assets/plus-jakarta-sans-latin-ext-wght-normal.woff2") format("woff2");
  unicode-range:U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C0,U+2113,U+2C60-2C7F,U+A720-A7FF}
@font-face{font-family:"Plus Jakarta Sans";font-style:normal;font-display:swap;font-weight:200 800;
  src:url("assets/plus-jakarta-sans-latin-wght-normal.woff2") format("woff2");
  unicode-range:U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD}
:root{
  --radius:.75rem;--background:#141417;--foreground:#e8e8ec;--card:#1c1c20;--surface-raised:#28282e;
  --primary:#e8e8ec;--primary-foreground:#141417;--secondary:#232328;--secondary-foreground:#d0d0d6;
  --muted-foreground:#a4a4ae;--accent:#232328;--destructive:#ef4444;--border:#28282e;--input:#1c1c20;--ring:#e8e8ec;
  --success:oklch(.75 .18 145);--warning:oklch(.8 .15 85);--info:oklch(.7 .15 250);
  color-scheme:dark;
}
*{box-sizing:border-box;border-color:var(--border)}
html{font-size:100%}
body{margin:0;background:var(--background);color:var(--foreground);
  font-family:"Plus Jakarta Sans",sans-serif;font-size:.875rem;line-height:1.5;-webkit-font-smoothing:antialiased}
a{color:inherit}
.page{max-width:56rem;margin:0 auto;padding:2rem 1.5rem 4rem}
@media (max-width:640px){.page{padding:1.25rem 1rem 3rem}}
.crumb{display:inline-flex;align-items:center;gap:.375rem;color:var(--muted-foreground);text-decoration:none;font-size:.8125rem;margin-bottom:1rem}
.crumb:hover{color:var(--foreground)}
.head{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:1rem;margin-bottom:1.5rem}
h1{font-size:1.5rem;font-weight:700;letter-spacing:-.015em;margin:0}
.sub{color:var(--muted-foreground);margin:.25rem 0 0}
.card{background:var(--card);border:1px solid var(--border);border-radius:var(--radius);padding:1.5rem;margin-bottom:1rem}
.card h2{font-size:1rem;font-weight:600;margin:0 0 .25rem}
.card .desc{color:var(--muted-foreground);margin:0 0 1.25rem}
.badge{display:inline-flex;align-items:center;gap:.375rem;border-radius:9999px;padding:.25rem .75rem;font-size:.75rem;font-weight:600;
  background:var(--secondary);color:var(--secondary-foreground);white-space:nowrap}
.badge .dot{width:.5rem;height:.5rem;border-radius:9999px;background:var(--muted-foreground)}
.badge.ok .dot{background:var(--success)} .badge.busy .dot{background:var(--info);animation:pulse 1.6s ease-in-out infinite}
.badge.warn .dot{background:var(--warning)} .badge.bad .dot{background:var(--destructive)}
@keyframes pulse{50%{opacity:.35}}
.stats{display:grid;grid-template-columns:repeat(auto-fill,minmax(9.5rem,1fr));gap:.75rem;margin-bottom:1rem}
@media (max-width:640px){.stats{grid-template-columns:repeat(2,minmax(0,1fr));gap:.5rem}.card{padding:1.25rem}}
.stat{background:var(--secondary);border-radius:calc(var(--radius) - 2px);padding:.875rem 1rem}
.stat .n{font-size:1.375rem;font-weight:700;letter-spacing:-.01em;font-variant-numeric:tabular-nums}
.stat .l{color:var(--muted-foreground);font-size:.75rem}
.stat.bad .n{color:var(--destructive)}
.meta{color:var(--muted-foreground);font-size:.8125rem}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:.5rem;height:2.25rem;padding:0 1rem;border-radius:calc(var(--radius) - 2px);
  font:inherit;font-weight:500;cursor:pointer;text-decoration:none;border:1px solid transparent;transition:background-color .15s,opacity .15s;white-space:nowrap}
.btn:focus-visible{outline:none;box-shadow:0 0 0 3px color-mix(in srgb,var(--ring) 50%,transparent)}
.btn-primary{background:var(--primary);color:var(--primary-foreground)}
.btn-primary:hover{background:color-mix(in srgb,var(--primary) 90%,transparent)}
.btn-outline{background:transparent;border-color:var(--border);color:var(--foreground)}
.btn-outline:hover{background:var(--accent)}
.btn-destructive{background:var(--destructive);color:#fff}
.btn-destructive:hover{background:color-mix(in srgb,var(--destructive) 90%,transparent)}
.btn-sm{height:2rem;padding:0 .75rem;font-size:.8125rem}
.btn[disabled]{opacity:.5;pointer-events:none}
.code{display:flex;gap:.5rem;align-items:stretch}
.code code{flex:1;min-width:0;display:block;background:var(--input);border:1px solid var(--border);border-radius:calc(var(--radius) - 2px);
  padding:.5rem .75rem;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8125rem;overflow-x:auto;white-space:nowrap}
table{width:100%;border-collapse:collapse}
th{text-align:left;font-weight:500;color:var(--muted-foreground);font-size:.75rem;padding:.5rem .75rem;border-bottom:1px solid var(--border)}
td{padding:.625rem .75rem;border-bottom:1px solid var(--border);vertical-align:middle}
tr:last-child td{border-bottom:0}
tbody tr:hover td{background:color-mix(in srgb,var(--secondary) 50%,transparent)}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
.check{display:inline-flex;width:1.25rem;height:1.25rem;border-radius:9999px;align-items:center;justify-content:center;font-size:.75rem;font-weight:700}
.check.ok{background:color-mix(in srgb,var(--success) 20%,transparent);color:var(--success)}
.check.bad{background:color-mix(in srgb,var(--destructive) 20%,transparent);color:var(--destructive)}
.detail.bad{color:var(--destructive)}
input[type=checkbox]{width:1rem;height:1rem;accent-color:var(--primary);vertical-align:middle;margin:0}
label.opt{display:flex;gap:.625rem;align-items:flex-start;margin:.75rem 0;cursor:pointer}
label.opt input{margin-top:.15rem}
.callout{border:1px solid color-mix(in srgb,var(--warning) 40%,transparent);background:color-mix(in srgb,var(--warning) 8%,transparent);
  border-radius:calc(var(--radius) - 2px);padding:.75rem 1rem;margin:0 0 1rem}
.callout.bad{border-color:color-mix(in srgb,var(--destructive) 40%,transparent);background:color-mix(in srgb,var(--destructive) 8%,transparent)}
.notes{color:var(--muted-foreground);font-size:.8125rem;margin:1rem 0 0;padding-left:1.1rem}
.notes li{margin:.25rem 0}
details{margin-top:.75rem} summary{cursor:pointer;color:var(--muted-foreground)} summary:hover{color:var(--foreground)}
.errors{list-style:none;padding:0;margin:.75rem 0 0;max-height:18rem;overflow:auto}
.errors li{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.75rem;color:var(--destructive);padding:.375rem 0;border-bottom:1px solid var(--border);word-break:break-word}
.progress{height:.375rem;background:var(--secondary);border-radius:9999px;overflow:hidden;margin-top:.5rem}
.progress > div{height:100%;background:var(--primary);transition:width .3s}
.row{display:flex;flex-wrap:wrap;gap:.75rem;align-items:center}
.empty{color:var(--muted-foreground);margin:0}
.sub3{font-size:.875rem;font-weight:600;margin:1.5rem 0 .25rem}
.tablewrap{overflow-x:auto;margin:0 -.25rem;padding:0 .25rem}
td select{max-width:14rem}
select{background:var(--input);color:var(--foreground);border:1px solid var(--border);border-radius:calc(var(--radius) - 4px);height:2rem;padding:0 .5rem;font:inherit}
input[type=radio]{width:1rem;height:1rem;accent-color:var(--primary);margin:0}
.cardhead{display:flex;gap:1rem;align-items:flex-start;justify-content:space-between;margin-bottom:1rem}
.cardhead .desc{margin:0}
.checks{display:grid;grid-template-columns:auto 12rem 1fr;align-items:baseline}
.checks > div{padding:.625rem .75rem;border-bottom:1px solid var(--border)}
.checks > div:nth-last-child(-n+3){border-bottom:0}
.checks .name{font-weight:500}
@media (max-width:640px){.checks{grid-template-columns:auto 1fr}.checks .name{border-bottom:0!important;padding-bottom:0}
  .checks .detail{grid-column:2;padding-top:.125rem}.checks .icon{grid-row:span 2}}
</style>
<script>
// Apply Silo's admin accent color, as the Silo web app does.
(function(){
  function lum(h){var v=parseInt(h.slice(1),16);function c(s){var x=((v>>s)&255)/255;return x<=.04045?x/12.92:Math.pow((x+.055)/1.055,2.4)}
    return .2126*c(16)+.7152*c(8)+.0722*c(0)}
  fetch("/api/v2/theme/branding",{credentials:"same-origin"}).then(function(r){return r.ok?r.json():null}).then(function(b){
    var m=b&&/^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec((b.accent_color||"").trim()); if(!m)return;
    var d=m[1].toLowerCase(); if(d.length===3)d=d.split("").map(function(x){return x+x}).join(""); var hex="#"+d;
    var s=document.documentElement.style; s.setProperty("--primary",hex); s.setProperty("--ring",hex);
    var bg=lum(hex), dark=lum("#141417");
    if((1.05)/(bg+.05)>(bg+.05)/(dark+.05)) s.setProperty("--primary-foreground","#ffffff");
  }).catch(function(){});
})();
</script>
</head><body><main class="page">`

const pageFoot = `<div id="expired" class="callout bad" hidden style="position:sticky;bottom:1rem;margin-top:1rem;background:color-mix(in srgb,var(--destructive) 14%,var(--card));box-shadow:0 10px 15px -3px rgb(0 0 0 / .4)">
  <strong>This page's access has expired.</strong> Silo lets a plugin page work for five minutes each time you open it.
  <a class="btn btn-primary btn-sm" style="margin-left:.5rem" href="/admin/plugins/community.kometa-sync">Reopen from Silo</a>
</div>
<script>
// Show times in the viewer's own time zone.
(function(){
  var now=new Date();
  document.querySelectorAll("time[data-local]").forEach(function(t){
    var d=new Date(t.getAttribute("datetime")); if(isNaN(d)) return;
    var o={weekday:"short",month:"short",day:"numeric",hour:"numeric",minute:"2-digit",timeZoneName:"short"};
    if(d.getFullYear()!==now.getFullYear()) o.year="numeric";
    if(t.hasAttribute("data-time-only")){ o={hour:"numeric",minute:"2-digit",timeZoneName:"short"}; }
    t.textContent=new Intl.DateTimeFormat(undefined,o).format(d);
  });
})();
// While something runs, poll its progress and reload when it's done.
(function(){
  var live=document.getElementById("live"); if(!live) return;
  var fmt=new Intl.NumberFormat(), timer;
  function tick(){
    fetch("progress",{credentials:"same-origin",cache:"no-store"}).then(function(r){
      if(!r.ok) throw new Error(r.status); return r.json();
    }).then(function(p){
      if(!p.running){ clearInterval(timer); location.reload(); return; }
      document.getElementById("live-phase").textContent=p.phase||"Working";
      document.getElementById("live-detail").textContent=p.detail||"";
      document.getElementById("live-count").textContent=p.total?fmt.format(p.done)+" of "+fmt.format(p.total):"";
      document.getElementById("live-bar").style.width=(p.total?Math.min(100,Math.floor(p.done*100/p.total)):0)+"%";
    }).catch(function(){ clearInterval(timer); });
  }
  timer=setInterval(tick,3000);
})();
</script>
<script>
// Silo issues plugin pages a five-minute access cookie when they are opened
// from Silo (the link it opens carries ?theme=). Reloads don't renew it, so
// remember when this visit started and warn before buttons stop working.
(function(){
  var KEY="kometa-sync-opened", TTL=5*60*1000, MARGIN=15*1000, u=new URL(location.href);
  try{
    if(u.searchParams.has("theme")){ sessionStorage.setItem(KEY,String(Date.now())); u.searchParams.delete("theme"); history.replaceState(null,"",u.pathname+u.search+u.hash); }
    var opened=Number(sessionStorage.getItem(KEY)||0);
  }catch(e){ return; }
  if(!opened) return;
  function expire(){
    var el=document.getElementById("expired"); if(el) el.hidden=false;
    document.querySelectorAll("main button[type=submit], main a[href^='?']").forEach(function(b){ b.setAttribute("disabled",""); b.style.pointerEvents="none"; b.style.opacity=".5"; });
  }
  var left=opened+TTL-MARGIN-Date.now();
  if(left<=0) expire(); else setTimeout(expire,left);
})();
</script>
</main></body></html>`

// thousands formats 13633 as "13,633".
func thousands(v any) string {
	var n int64
	switch t := v.(type) {
	case int:
		n = int64(t)
	case int64:
		n = t
	default:
		return fmt.Sprint(v)
	}
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := strconv.FormatInt(n, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}

var statusPage = template.Must(template.New("status").Funcs(template.FuncMap{"num": thousands, "when": localTime, "took": took, "pct": pct}).Parse(pageHead + `
<a class="crumb" href="/admin/plugins/community.kometa-sync">&#8592; Plugin settings</a>
<div class="head">
  <div><h1>Plex Artwork Sync</h1><p class="sub">Keeps Silo's movie and series posters and collections matching Plex.</p></div>
  {{if .Running}}<span class="badge busy"><span class="dot"></span>{{if eq .Mode "revert"}}Reverting{{else if eq .Mode "reset"}}Resetting collections{{else}}Syncing{{end}}</span>
  {{else if .Due}}<span style="display:inline-flex;gap:.5rem;align-items:center"><span class="badge warn"><span class="dot"></span>Sync at&nbsp;{{when .Due}}</span>
    <form method="post" action="cancel" style="margin:0"><input type="hidden" name="token" value="{{.FormToken}}"><button class="btn btn-outline btn-sm" type="submit">Skip</button></form></span>
  {{else if .ConfigError}}<span class="badge bad"><span class="dot"></span>Not configured</span>
  {{else}}<span class="badge ok"><span class="dot"></span>Ready</span>{{end}}
</div>

{{if .ConfigError}}<div class="callout bad">{{.ConfigError}}</div>{{end}}

{{if .Running}}{{with .Progress}}<section class="card" id="live">
  <div class="cardhead" style="margin-bottom:.5rem"><div><h2 id="live-phase">{{.Phase}}</h2>
    <p class="desc" id="live-detail">{{.Detail}}</p></div>
    <span class="meta" id="live-count">{{if .Total}}{{num .Done}} of {{num .Total}}{{end}}</span></div>
  <div class="progress"><div id="live-bar" style="width:{{pct .Done .Total}}%"></div></div>
  <div class="cardhead" style="margin:.75rem 0 0;align-items:center">
    <p class="meta" style="margin:0">Started {{when .Started}}. This updates by itself.</p>
    <form method="post" action="cancel" onsubmit="return confirm('Stop this run? What it has finished so far is kept.')">
      <input type="hidden" name="token" value="{{$.FormToken}}">
      <button class="btn btn-outline btn-sm" type="submit">Cancel</button>
    </form>
  </div>
</section>{{end}}{{end}}

<section class="card">
  <h2>Libraries</h2>
  <p class="desc">Choose which Plex libraries to sync into Silo. Only movie and TV libraries are listed. Leaving everything unticked pauses syncing.</p>
  {{if .LibrariesError}}<p class="empty">{{.LibrariesError}}</p>{{else}}
  <form method="post" action="libraries">
    <input type="hidden" name="token" value="{{.FormToken}}">
    {{$v := .Libraries}}
    <div class="tablewrap"><table><thead><tr><th style="width:2rem"></th><th>Plex library</th><th>Collections go to</th><th class="prio" style="width:6rem">Priority</th></tr></thead><tbody>
    {{range $v.Options}}<tr><td><input type="checkbox" name="library" value="{{.ID}}" id="sel-{{.ID}}"{{if .Selected}} checked{{end}}></td>
      <td><label for="sel-{{.ID}}">{{.Title}}</label><div class="meta">{{.Kind}}</div></td>
      <td><select name="map_{{.ID}}" aria-label="Silo library for {{.Title}} collections">
        <option value="0"{{if eq .Mapped 0}} selected{{end}}>{{if .AutoName}}Automatic ({{.AutoName}}){{else}}Automatic (no match){{end}}</option>
        {{$m := .Mapped}}{{range .MapChoices}}<option value="{{.ID}}"{{if eq .ID $m}} selected{{end}}>{{.Name}}</option>{{end}}
      </select></td>
      <td class="prio"><select name="priority_{{.ID}}" aria-label="Priority for {{.Title}}">{{$p := .Priority}}{{range $v.Positions}}<option value="{{.}}"{{if eq . $p}} selected{{end}}>{{.}}</option>{{end}}</select></td></tr>{{end}}
    </tbody></table></div>
    {{if $v.MapError}}<p class="meta detail bad">{{$v.MapError}}</p>{{end}}
    <p class="meta" style="margin:.5rem 0 0">"Collections go to" is the Silo library each Plex library's collections are placed in. Automatic matches by name. With no match, a collection goes wherever its titles are.</p>
    <h3 class="sub3">Collections with the same name in more than one library</h3>
    <p class="meta" style="margin:0 0 .5rem">For example "Marvel" in both Movies and Movies 4k. In Silo a title in both libraries is one item with two versions, so both collections mostly hold the same titles.</p>
    <label class="opt"><input type="radio" name="collection_mode" value="separate"{{if ne $v.CollMode "combined"}} checked{{end}}><span><strong>Separate</strong><br><span class="meta">One Silo collection per Plex library, each in its own Silo library. Browsing the 4K library shows only titles you have in 4K, but places that list all collections show both.</span></span></label>
    <label class="opt"><input type="radio" name="collection_mode" value="combined"{{if eq $v.CollMode "combined"}} checked{{end}}><span><strong>Combined</strong><br><span class="meta">One Silo collection, shown in each of those libraries, holding every title from both. Its poster comes from the library chosen below. Opened from the 4K library, it also lists titles you only have in 1080p.</span></span></label>
    <p class="meta" style="margin:.25rem 0 0">Switching moves existing collections on the next sync instead of rebuilding them.</p>
    <h3 class="sub3">When a title is in more than one library</h3>
    <p class="meta" style="margin:0 0 .5rem">For example the same movie in Movies and Movies 4k. Silo has one poster per title, so one copy's poster is used. Combined collections take their poster the same way.</p>
    <label class="opt"><input type="radio" name="winner" value="highest"{{if eq $v.Winner "highest"}} checked{{end}}><span><strong>Highest resolution</strong><br><span class="meta">Movies compare the actual files. TV shows go by library name: one with 4K, UHD or 2160 in its name wins.</span></span></label>
    <label class="opt"><input type="radio" name="winner" value="lowest"{{if eq $v.Winner "lowest"}} checked{{end}}><span><strong>Lowest resolution</strong><br><span class="meta">The same rules, reversed.</span></span></label>
    <label class="opt"><input type="radio" name="winner" value="priority"{{if eq $v.Winner "priority"}} checked{{end}}><span><strong>Library priority</strong><br><span class="meta">The library with the lowest priority number wins. Set the numbers in the table above.</span></span></label>
    <div class="row" style="margin-top:1rem"><button class="btn btn-primary btn-sm" type="submit">Save</button>
      <button class="btn btn-outline btn-sm" type="button" onclick="this.form.querySelectorAll('input[name=library]').forEach(c=>c.checked=true)">Select all</button>
      <button class="btn btn-outline btn-sm" type="button" onclick="this.form.querySelectorAll('input[name=library]').forEach(c=>c.checked=false)">Clear all</button></div>
    <p class="meta" style="margin:.75rem 0 0">Changing this choice re-sends the winning poster for every title that's in more than one library, once, on the next sync.</p>
  </form>
  <script>
  (function(){var f=document.querySelector('form[action=libraries]');if(!f)return;
    function sync(){var on=f.querySelector('input[name=winner][value=priority]').checked;f.querySelectorAll('.prio').forEach(function(e){e.style.display=on?'':'none'})}
    f.querySelectorAll('input[name=winner]').forEach(function(r){r.addEventListener('change',sync)});sync();})();
  </script>{{end}}
</section>

<section class="card">
  <h2>Last run</h2>
  {{with .Last}}
  <p class="desc">{{when .StartedAt}}{{if .FinishedAt}} · took {{took .StartedAt .FinishedAt}}{{end}} · {{.Trigger}}{{if .DryRun}} · dry run (counts show what would change){{end}}{{if .Cancelled}} · <strong>cancelled</strong>{{end}}</p>
  {{if .Note}}<div class="callout">{{.Note}}</div>{{end}}
  <div class="stats">
    {{if .PostersReverted}}<div class="stat"><div class="n">{{num .PostersReverted}}</div><div class="l">Posters reverted</div></div>{{else}}
    <div class="stat"><div class="n">{{num .PlexItems}}</div><div class="l">Plex items checked</div></div>
    <div class="stat"><div class="n">{{num .PostersApplied}}</div><div class="l">Posters {{if .DryRun}}to apply{{else}}applied{{end}}</div></div>
    <div class="stat"><div class="n">{{num .PostersUnchanged}}</div><div class="l">Unchanged</div></div>
    <div class="stat"><div class="n">{{num .NotInSilo}}</div><div class="l">Not in Silo</div></div>
    <div class="stat"><div class="n">{{num .NoTMDBID}}</div><div class="l">No TMDB id</div></div>
    {{if .DuplicatesSkipped}}<div class="stat"><div class="n">{{num .DuplicatesSkipped}}</div><div class="l">Copies in other libraries</div></div>{{end}}
    {{if .CollectionsChecked}}<div class="stat"><div class="n">{{num .CollectionsChecked}}</div><div class="l">Collections checked</div></div>{{end}}
    <div class="stat"><div class="n">{{num .CollectionsCreated}} / {{num .CollectionsUpdated}}</div><div class="l">Collections created / updated</div></div>{{end}}
    <div class="stat"><div class="n">{{num .CollectionsDeleted}}</div><div class="l">Collections deleted</div></div>
    <div class="stat{{if .ErrorCount}} bad{{end}}"><div class="n">{{num .ErrorCount}}</div><div class="l">Errors</div></div>
  </div>
  {{if .Errors}}<details{{if le (len .Errors) 3}} open{{end}}><summary>Show errors{{if gt .ErrorCount (len .Errors)}} (first {{len .Errors}} of {{.ErrorCount}}){{end}}</summary>
    <ul class="errors">{{range .Errors}}<li>{{.}}</li>{{end}}</ul></details>{{end}}
  {{else}}<p class="empty">No runs yet. Use Run now on the Plex Artwork Sync task in Silo's scheduled tasks, or wait for Kometa's next run if you use the webhook.</p>{{end}}
</section>

<section class="card">
  <h2>Kometa webhook <span class="meta">(optional)</span></h2>
  <p class="desc">If you use Kometa, this starts a sync after every Kometa run. Without Kometa, give the Plex Artwork Sync task a schedule in Silo's scheduled tasks instead.</p>
  <p class="desc">Add this under <code>webhooks: run_end:</code> in Kometa's config.yml. Replace <code>SILO_HOST</code> with the address Kometa uses to reach Silo (the container name and port if they share a Docker network, otherwise the server's address) and the secret with your webhook secret.</p>
  <div class="code"><code id="hook">{{.WebhookURL}}</code><button class="btn btn-outline btn-sm" type="button"
    onclick="navigator.clipboard&&navigator.clipboard.writeText(document.getElementById('hook').textContent).then(()=>{this.textContent='Copied';setTimeout(()=>this.textContent='Copy',1500)})">Copy</button></div>
</section>

<section class="card">
  <div class="cardhead"><div><h2>Connection check</h2>
    <p class="desc">Tests Plex, Kometa's Overlay label (when that setting is on) and the Silo API key. Nothing is written.</p></div>
    <a class="btn btn-outline btn-sm" href="?check=1">{{if .Checks}}Run again{{else}}Run check{{end}}</a></div>
  {{if .Checks}}<div class="checks">{{range .Checks}}
    <div class="icon"><span class="check {{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}&#10003;{{else}}&#10005;{{end}}</span></div>
    <div class="name">{{.Name}}</div><div class="detail{{if not .OK}} bad{{end}}">{{.Detail}}</div>{{end}}</div>{{end}}
</section>

{{with .Libraries}}<section class="card">
  <h2>Reset collections</h2>
  <p class="desc">Deletes the Silo collections this plugin made for the chosen libraries, including any it lost track of, and leaves posters alone. The next sync rebuilds them in the Silo library set under "Collections go to". A combined collection is reset if any library it draws on is chosen.</p>
  <form method="post" action="reset-collections">
    <input type="hidden" name="token" value="{{$.FormToken}}">
    <table><thead><tr><th style="width:2rem"></th><th>Plex library</th><th class="num">Collections on record</th></tr></thead><tbody>
    {{range .Options}}<tr><td><input type="checkbox" name="library" value="{{.ID}}" id="rc-{{.ID}}"></td>
      <td><label for="rc-{{.ID}}">{{.Title}}</label></td><td class="num">{{num (index $.CollectionCounts .ID)}}</td></tr>{{end}}
    </tbody></table>
    <label class="opt"><input type="checkbox" name="confirm"><span>I understand this deletes these collections in Silo</span></label>
    <button class="btn btn-destructive" type="submit"{{if $.Running}} disabled{{end}}>Reset collections</button>
  </form>
</section>{{end}}

<section class="card">
  <h2>Revert</h2>
  {{if .Tracked}}
  <p class="desc">Puts the posters this plugin applied back to Silo's default and stops tracking them. For each item it removes only the image lock, keeping any other locks you set, then runs Silo's own metadata refresh, a few at a time.</p>
  <div class="callout">Untick the library under <strong>Libraries</strong> and save first, or the next sync pushes the posters again.</div>
  <form method="post" action="revert">
    <input type="hidden" name="token" value="{{.FormToken}}">
    <table><thead><tr><th style="width:2rem"></th><th>Library</th><th class="num">Posters</th><th class="num">Collections</th></tr></thead><tbody>
    {{range .Tracked}}<tr><td><input type="checkbox" name="library" value="{{.ID}}" id="lib-{{.ID}}"></td>
      <td><label for="lib-{{.ID}}">{{.Title}}</label></td><td class="num">{{num .Posters}}</td><td class="num">{{num .Collections}}</td></tr>{{end}}
    </tbody></table>
    <label class="opt"><input type="checkbox" name="delete_collections"><span>Also delete the Silo collections this plugin created for these libraries</span></label>
    <label class="opt"><input type="checkbox" name="confirm"><span>I understand this resets these posters in Silo</span></label>
    <button class="btn btn-destructive" type="submit"{{if .Running}} disabled{{end}}>Revert selected libraries</button>
  </form>
  <ul class="notes">
    <li>A movie in two libraries (for example Movies and Movies 4k) is one item in Silo, so reverting either library resets it. If the other library is still synced, its poster comes back on the next run.</li>
    <li>If you hand-picked a poster in Silo before this plugin replaced it, revert gives you Silo's default, not that earlier pick.</li>
  </ul>
  {{else}}<p class="empty">Nothing to revert yet. This plugin hasn't applied any posters or created any collections. Dry runs don't count.</p>{{end}}
</section>
` + pageFoot))

var messagePage = template.Must(template.New("msg").Parse(pageHead + `
<a class="crumb" href="status">&#8592; Status</a>
<section class="card"><p style="margin:0 0 1rem">{{.}}</p><a class="btn btn-primary btn-sm" href="status">Back to status</a></section>
<script>setTimeout(function(){location.href="status"},3000)</script>
` + pageFoot))

// localTime renders a timestamp the page's script shows in the viewer's time
// zone; without script it reads as UTC.
func localTime(rfc string) template.HTML {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return template.HTML(template.HTMLEscapeString(rfc))
	}
	return template.HTML(`<time data-local datetime="` + t.UTC().Format(time.RFC3339) + `">` +
		t.UTC().Format("Mon, Jan 2, 3:04 PM") + ` UTC</time>`)
}

// took formats the time between two timestamps, e.g. "3m 1s".
func took(start, end string) string {
	a, err1 := time.Parse(time.RFC3339, start)
	b, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil || b.Before(a) {
		return ""
	}
	d := b.Sub(a).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func pct(done, total int) int {
	if total <= 0 {
		return 0
	}
	return min(100, done*100/total)
}
