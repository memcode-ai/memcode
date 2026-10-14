package sitelook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Report is a page's measured design system.
type Report struct {
	URL      string      `json:"-"`
	Viewport Viewport    `json:"-"`
	Title    string      `json:"title"`
	Height   int         `json:"height"`
	Fonts    []string    `json:"fonts"`
	Type     []TypeStyle `json:"type"`
	Surfaces []Share     `json:"surfaces"`
	Text     []Share     `json:"text"`
	Accents  []Share     `json:"accents"`
	Layout   Layout      `json:"layout"`
	Sections []Section   `json:"sections"`
	Motion   Motion      `json:"motion"`
	Media    Media       `json:"media"`
	Errors   []string    `json:"-"`
}

// TypeStyle is one distinct text style in use.
type TypeStyle struct {
	Tag       string  `json:"tag"`
	Family    string  `json:"family"`
	Size      float64 `json:"size"`
	Weight    string  `json:"weight"`
	Line      float64 `json:"line"`   // line-height ÷ size (0 = normal)
	Track     float64 `json:"track"`  // letter-spacing in em
	Case      string  `json:"case"`   // text-transform when not none
	Italic    bool    `json:"italic"` // font-style italic
	Color     string  `json:"color"`
	Chars     int     `json:"chars"`
	Sample    string  `json:"sample"`
	MaxWidth  int     `json:"measure"` // widest line box seen, px
	Alignment string  `json:"align"`
}

// Share is a color and how much of the page it covers.
type Share struct {
	Color string  `json:"color"`
	Pct   float64 `json:"pct"`
}

// Layout is the page's spatial system.
type Layout struct {
	ContentWidth  int      `json:"contentWidth"`
	Gutter        int      `json:"gutter"`
	SectionPad    []string `json:"sectionPad"`
	ButtonRadius  string   `json:"buttonRadius"`
	CardRadius    string   `json:"cardRadius"`
	Buttons       []string `json:"buttons"`
	Borders       string   `json:"borders"`
	Shadows       int      `json:"shadows"`
	StickyHeader  bool     `json:"stickyHeader"`
	HeaderHeight  int      `json:"headerHeight"`
	FullBleedImgs int      `json:"fullBleed"`
}

// Section is one band of the page, top to bottom.
type Section struct {
	Top     int    `json:"top"`
	Height  int    `json:"height"`
	BG      string `json:"bg"`
	Heading string `json:"heading"`
	Size    int    `json:"size"`
	Cols    int    `json:"cols"`
	Media   string `json:"media"`
	Tag     string `json:"tag"`
}

// Motion is how the page moves.
type Motion struct {
	Libraries    []string `json:"libs"`
	Animated     int      `json:"animated"`     // elements carrying scroll/reveal markers
	Transitions  int      `json:"transitions"`  // elements with CSS transitions
	Running      int      `json:"running"`      // Web Animations running now
	Autoplay     int      `json:"autoplay"`     // autoplaying videos
	SmoothScroll bool     `json:"smoothScroll"` // a smooth-scroll library or CSS
	Markers      []string `json:"markers"`      // attribute names used for reveals
}

// Media is the page's imagery.
type Media struct {
	Images int `json:"img"`
	Videos int `json:"video"`
	SVGs   int `json:"svg"`
	Canvas int `json:"canvas"`
	BGImgs int `json:"bgimg"`
}

// Inspect renders url at vp and measures its design system.
func Inspect(ctx context.Context, b Browser, url string, vp Viewport) (Report, error) {
	if err := load(ctx, b, url, vp); err != nil {
		return Report{}, err
	}
	raw, err := b.Eval(ctx, "(function(){"+inspectJS+"})()")
	if err != nil {
		return Report{}, fmt.Errorf("measuring the page: %w", err)
	}
	var r Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, fmt.Errorf("reading the measurements: %w", err)
	}
	r.URL, r.Viewport, r.Errors = url, vp, consoleErrors(b)
	return r, nil
}

// Format renders the report compactly for the model.
func (r Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Design of %s at %s %dx%d (page %dpx tall)", r.URL, r.Viewport.Name, r.Viewport.Width, r.Viewport.Height, r.Height)
	if r.Title != "" {
		fmt.Fprintf(&b, ", title %q", r.Title)
	}
	b.WriteString("\n")
	if len(r.Fonts) > 0 {
		b.WriteString("\nFonts loaded: " + strings.Join(r.Fonts, ", ") + "\n")
	}
	if len(r.Type) > 0 {
		b.WriteString("\nType scale (largest first; size/line-height, tracking, weight, color, how much text):\n")
		for _, t := range r.Type {
			line := fmt.Sprintf("  %-7s %s %gpx", t.Tag, t.Family, t.Size)
			if t.Line > 0 {
				line += fmt.Sprintf("/%.2f", t.Line)
			}
			line += " w" + t.Weight
			if t.Track != 0 {
				line += fmt.Sprintf(" track %+.3fem", t.Track)
			}
			if t.Case != "" {
				line += " " + t.Case
			}
			if t.Italic {
				line += " italic"
			}
			if t.Alignment != "" && t.Alignment != "start" && t.Alignment != "left" {
				line += " " + t.Alignment
			}
			line += " " + t.Color
			if t.MaxWidth > 0 {
				line += fmt.Sprintf(" measure %dpx", t.MaxWidth)
			}
			line += fmt.Sprintf(" (%d chars)", t.Chars)
			if t.Sample != "" {
				line += fmt.Sprintf(" %q", t.Sample)
			}
			b.WriteString(line + "\n")
		}
	}
	writeShares := func(label string, s []Share) {
		if len(s) == 0 {
			return
		}
		parts := make([]string, len(s))
		for i, x := range s {
			parts[i] = fmt.Sprintf("%s %.0f%%", x.Color, x.Pct)
		}
		b.WriteString(label + strings.Join(parts, ", ") + "\n")
	}
	b.WriteString("\n")
	writeShares("Surfaces (share of page area): ", r.Surfaces)
	writeShares("Text colors (share of text): ", r.Text)
	writeShares("Accents (small filled elements): ", r.Accents)
	l := r.Layout
	b.WriteString("\nLayout:")
	if l.ContentWidth > 0 {
		fmt.Fprintf(&b, " content width ~%dpx;", l.ContentWidth)
	}
	if l.Gutter > 0 {
		fmt.Fprintf(&b, " side gutter ~%dpx;", l.Gutter)
	}
	if len(l.SectionPad) > 0 {
		fmt.Fprintf(&b, " section padding %s;", strings.Join(l.SectionPad, ", "))
	}
	if l.StickyHeader {
		fmt.Fprintf(&b, " sticky header %dpx;", l.HeaderHeight)
	} else if l.HeaderHeight > 0 {
		fmt.Fprintf(&b, " header %dpx;", l.HeaderHeight)
	}
	if l.ButtonRadius != "" {
		fmt.Fprintf(&b, " button radius %s;", l.ButtonRadius)
	}
	if l.CardRadius != "" {
		fmt.Fprintf(&b, " card radius %s;", l.CardRadius)
	}
	if l.Borders != "" {
		fmt.Fprintf(&b, " rules/borders %s;", l.Borders)
	}
	fmt.Fprintf(&b, " %d shadowed elements; %d full-bleed images/videos\n", l.Shadows, l.FullBleedImgs)
	for _, btn := range l.Buttons {
		b.WriteString("  button: " + btn + "\n")
	}
	if len(r.Sections) > 0 {
		b.WriteString("\nSections (top to bottom):\n")
		for i, s := range r.Sections {
			line := fmt.Sprintf("  %2d. %5d–%-5d %-8s %s", i+1, s.Top, s.Top+s.Height, "<"+s.Tag+">", s.BG)
			if s.Cols > 1 {
				line += fmt.Sprintf(" %d-up", s.Cols)
			}
			if s.Media != "" {
				line += " [" + s.Media + "]"
			}
			if s.Heading != "" {
				line += fmt.Sprintf(" %dpx %q", s.Size, s.Heading)
			}
			b.WriteString(line + "\n")
		}
	}
	m := r.Motion
	b.WriteString("\nMotion:")
	if len(m.Libraries) > 0 {
		b.WriteString(" " + strings.Join(m.Libraries, ", ") + ";")
	} else {
		b.WriteString(" no animation library detected;")
	}
	if m.SmoothScroll {
		b.WriteString(" smooth scrolling;")
	}
	fmt.Fprintf(&b, " %d elements marked for scroll reveals", m.Animated)
	if len(m.Markers) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(m.Markers, ", "))
	}
	fmt.Fprintf(&b, "; %d with CSS transitions; %d animations running; %d autoplay videos\n", m.Transitions, m.Running, m.Autoplay)
	md := r.Media
	fmt.Fprintf(&b, "Media: %d images, %d videos, %d svg, %d canvas, %d background images\n", md.Images, md.Videos, md.SVGs, md.Canvas, md.BGImgs)
	if len(r.Errors) > 0 {
		b.WriteString("\nConsole errors:\n  " + strings.Join(r.Errors, "\n  ") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// inspectJS measures the rendered page. It reads computed styles and layout
// only; it changes nothing but its own scratch canvas.
const inspectJS = `
var vw = window.innerWidth, vh = window.innerHeight;
var se = document.scrollingElement || document.documentElement;
var pageH = Math.max(se.scrollHeight, document.body ? document.body.scrollHeight : 0);
var sy = window.scrollY;
var cv = document.createElement('canvas'); cv.width = cv.height = 1;
var cx = cv.getContext('2d', {willReadFrequently: true});
var cache = {};
function hex(c) {
  if (!c || c === 'transparent') return null;
  if (cache[c] !== undefined) return cache[c];
  cx.clearRect(0, 0, 1, 1); cx.fillStyle = 'rgba(0,0,0,0)'; cx.fillStyle = c; cx.fillRect(0, 0, 1, 1);
  var d = cx.getImageData(0, 0, 1, 1).data, r = null;
  if (d[3] > 8) {
    r = '#' + [d[0], d[1], d[2]].map(function(x){ var s = x.toString(16); return s.length < 2 ? '0' + s : s; }).join('');
    if (d[3] < 250) r += '/' + Math.round(d[3] / 2.55) + '%';
  }
  cache[c] = r; return r;
}
function px(v) { var n = parseFloat(v); return isNaN(n) ? 0 : n; }
function fam(f) { return (f || '').split(',')[0].replace(/["']/g, '').trim(); }
function top(m, n, total) {
  var arr = Object.keys(m).map(function(k){ return [k, m[k]]; }).sort(function(a, b){ return b[1] - a[1]; }).slice(0, n);
  return arr.map(function(a){ return {color: a[0], pct: total ? Math.round(a[1] / total * 1000) / 10 : 0}; });
}
function mode(m) { var best = '', bc = 0; for (var k in m) if (m[k] > bc) { best = k; bc = m[k]; } return best; }
function clip(s, n) { s = (s || '').replace(/\s+/g, ' ').trim(); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
function visible(el, cs) { return cs.display !== 'none' && cs.visibility !== 'hidden'; }
function effBG(el) {
  for (var e = el; e && e.nodeType === 1; e = e.parentElement) { var c = hex(getComputedStyle(e).backgroundColor); if (c && c.indexOf('/') < 0) return c; }
  return hex(getComputedStyle(document.documentElement).backgroundColor) || '#ffffff';
}

var all = document.body ? document.body.getElementsByTagName('*') : [];
var styles = {}, surf = {}, surfTotal = 0, txt = {}, txtTotal = 0, acc = {}, accTotal = 0;
var mw = {}, btnR = {}, cardR = {}, btns = {}, borders = {}, shadows = 0, trans = 0, marked = 0, markers = {}, bgimgs = 0, fullBleed = 0;
var lefts = [];
var markerAttrs = ['data-animate', 'data-aos', 'data-scroll', 'data-speed', 'data-w-id', 'data-sal', 'data-reveal', 'data-framer-appear-id', 'data-motion'];
for (var i = 0; i < all.length && i < 8000; i++) {
  var el = all[i], cs = getComputedStyle(el);
  if (!visible(el, cs)) continue;
  var r = el.getBoundingClientRect();
  if (r.width < 1 || r.height < 1) continue;
  var w = Math.min(r.width, vw), h = Math.min(r.height, pageH);
  var bg = hex(cs.backgroundColor);
  if (bg) {
    if (r.width >= vw * 0.3) { surf[bg] = (surf[bg] || 0) + w * h; surfTotal += w * h; }
    else if (r.width < 400 && r.height < 120) { acc[bg] = (acc[bg] || 0) + w * h; accTotal += w * h; }
  }
  if (cs.backgroundImage && cs.backgroundImage !== 'none' && cs.backgroundImage.indexOf('url(') >= 0) bgimgs++;
  if (cs.boxShadow && cs.boxShadow !== 'none') shadows++;
  if (cs.transitionDuration && cs.transitionDuration.split(',').some(function(d){ return parseFloat(d) > 0; })) trans++;
  for (var a = 0; a < markerAttrs.length; a++) if (el.hasAttribute(markerAttrs[a])) { marked++; markers[markerAttrs[a]] = 1; break; }
  if (cs.maxWidth !== 'none' && r.width > 320 && r.width < vw - 8) mw[Math.round(r.width)] = (mw[Math.round(r.width)] || 0) + 1;
  var bw = px(cs.borderTopWidth) || px(cs.borderBottomWidth);
  if (bw > 0 && r.width > vw * 0.4) { var bc = hex(cs.borderTopColor) || hex(cs.borderBottomColor); if (bc) borders[bw + 'px ' + bc] = (borders[bw + 'px ' + bc] || 0) + 1; }
  var tag = el.tagName.toLowerCase();
  if ((tag === 'img' || tag === 'video') && r.width >= vw * 0.95) fullBleed++;
  var isBtn = tag === 'button' || el.getAttribute('role') === 'button' || (tag === 'a' && (bg || bw > 0) && r.height >= 28 && r.height <= 80 && r.width < 420);
  if (isBtn && el.innerText && el.innerText.trim()) {
    btnR[cs.borderRadius] = (btnR[cs.borderRadius] || 0) + 1;
    var key = fam(cs.fontFamily) + ' ' + px(cs.fontSize) + 'px w' + cs.fontWeight + ', pad ' + cs.paddingTop + ' ' + cs.paddingRight + ', radius ' + cs.borderRadius + ', ' + (bg || 'no fill') + ' / text ' + hex(cs.color) + (bw ? ', border ' + bw + 'px ' + hex(cs.borderTopColor) : '');
    btns[key] = (btns[key] || 0) + 1;
  } else if ((bg || bw > 0 || (cs.boxShadow && cs.boxShadow !== 'none')) && r.width >= 200 && r.width <= 720 && r.height >= 140) {
    cardR[cs.borderRadius] = (cardR[cs.borderRadius] || 0) + 1;
  }
  var chars = 0, sample = '';
  for (var c = el.firstChild; c; c = c.nextSibling) if (c.nodeType === 3) { var t = c.textContent.trim(); if (t) { chars += t.length; if (!sample) sample = t; } }
  if (!chars) continue;
  var fs = px(cs.fontSize), lh = cs.lineHeight === 'normal' ? 0 : px(cs.lineHeight) / fs, ls = cs.letterSpacing === 'normal' ? 0 : px(cs.letterSpacing) / fs;
  var col = hex(cs.color) || '';
  txt[col] = (txt[col] || 0) + chars; txtTotal += chars;
  var k = [fam(cs.fontFamily), fs, cs.fontWeight, lh.toFixed(2), ls.toFixed(3), cs.textTransform, cs.fontStyle].join('|');
  var s = styles[k];
  if (!s) s = styles[k] = {tag: tag, family: fam(cs.fontFamily), size: Math.round(fs * 10) / 10, weight: cs.fontWeight, line: Math.round(lh * 100) / 100, track: Math.round(ls * 1000) / 1000,
    'case': cs.textTransform === 'none' ? '' : cs.textTransform, italic: cs.fontStyle === 'italic', colors: {}, chars: 0, sample: clip(sample, 60), measure: 0, align: cs.textAlign};
  s.chars += chars; s.colors[col] = (s.colors[col] || 0) + chars;
  if (r.width > s.measure && r.width < vw) s.measure = Math.round(r.width);
  if (fs >= 15 && r.width < vw * 0.98) lefts.push(Math.round(r.left));
}
var type = Object.keys(styles).map(function(k){ var s = styles[k]; s.color = mode(s.colors); delete s.colors; return s; })
  .filter(function(s){ return s.chars >= 12 || s.size >= 28; })
  .sort(function(a, b){ return b.size - a.size || b.chars - a.chars; }).slice(0, 14);

// Sections: walk down single-child wrappers, then split tall wrappers.
function kids(el) { var out = []; for (var c = el.firstElementChild; c; c = c.nextElementSibling) { var cs = getComputedStyle(c); if (!visible(c, cs) || cs.position === 'fixed') continue; var r = c.getBoundingClientRect(); if (r.height >= 40 && r.width >= vw * 0.5) out.push(c); } return out; }
var spine = document.body;
for (var d = 0; d < 10; d++) { var ks = kids(spine); if (ks.length === 1) spine = ks[0]; else break; }
var secs = [];
function collect(el, depth) {
  var ks = kids(el);
  for (var i = 0; i < ks.length && secs.length < 30; i++) {
    var r = ks[i].getBoundingClientRect(), t = ks[i].tagName.toLowerCase();
    if (depth < 3 && (t === 'main' || (r.height > vh * 2.2 && kids(ks[i]).length >= 2))) collect(ks[i], depth + 1);
    else if (r.height >= 60) secs.push(ks[i]);
  }
}
collect(spine, 0);
var pads = {};
var sections = secs.map(function(el) {
  var r = el.getBoundingClientRect(), cs = getComputedStyle(el);
  pads[cs.paddingTop + '/' + cs.paddingBottom] = (pads[cs.paddingTop + '/' + cs.paddingBottom] || 0) + 1;
  var best = null, bs = 0, d = el.getElementsByTagName('*');
  for (var i = 0; i < d.length && i < 600; i++) {
    if (/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE)$/.test(d[i].tagName)) continue;
    var hasText = false; for (var c = d[i].firstChild; c; c = c.nextSibling) if (c.nodeType === 3 && c.textContent.trim()) { hasText = true; break; }
    if (!hasText) continue;
    var dcs = getComputedStyle(d[i]); if (!visible(d[i], dcs)) continue;
    var f = px(dcs.fontSize); if (f > bs) { bs = f; best = d[i]; }
  }
  var cols = 1;
  for (var i = 0; i < d.length && i < 600; i++) {
    var p = d[i], n = 0, tops = {};
    for (var c = p.firstElementChild; c; c = c.nextElementSibling) { var cr = c.getBoundingClientRect(); if (cr.width >= 120 && cr.height >= 40) { var tk = Math.round(cr.top / 6); tops[tk] = (tops[tk] || 0) + 1; } }
    for (var tk in tops) if (tops[tk] > cols) cols = tops[tk];
  }
  var m = [], im = el.getElementsByTagName('img').length, vi = el.getElementsByTagName('video').length, ca = el.getElementsByTagName('canvas').length;
  if (im) m.push(im + ' img'); if (vi) m.push(vi + ' video'); if (ca) m.push(ca + ' canvas');
  return {top: Math.round(r.top + sy), height: Math.round(r.height), bg: effBG(el), heading: best ? clip(best.innerText || best.textContent, 70) : '', size: Math.round(bs), cols: Math.min(cols, 8), media: m.join(', '), tag: el.tagName.toLowerCase()};
});

var header = document.querySelector('header, [role=banner], nav'), hh = 0, sticky = false;
if (header) { var hr = header.getBoundingClientRect(); hh = Math.round(hr.height); for (var e = header; e && e.nodeType === 1; e = e.parentElement) { var p = getComputedStyle(e).position; if (p === 'fixed' || p === 'sticky') { sticky = true; break; } } }
lefts.sort(function(a, b){ return a - b; });
var gutter = lefts.length ? lefts[Math.floor(lefts.length * 0.1)] : 0;
var cw = 0, cc = 0; for (var k in mw) if (mw[k] > cc || (mw[k] === cc && +k > cw)) { cw = +k; cc = mw[k]; }

var libs = [];
if (window.gsap) libs.push('GSAP ' + (window.gsap.version || ''));
if (window.ScrollTrigger || (window.gsap && window.gsap.plugins && window.gsap.plugins.scrollTrigger)) libs.push('ScrollTrigger');
var lenis = !!(window.lenis || window.Lenis || document.documentElement.classList.contains('lenis'));
if (lenis) libs.push('Lenis smooth scroll');
if (document.querySelector('[data-scroll-container]') || document.documentElement.classList.contains('has-scroll-smooth')) libs.push('Locomotive Scroll');
if (window.Webflow) libs.push('Webflow interactions');
if (document.querySelector('[data-framer-name],[data-framer-component-type]')) libs.push('Framer');
if (window.AOS || document.querySelector('[data-aos]')) libs.push('AOS');
if (window.THREE || document.querySelector('canvas[data-engine]')) libs.push('three.js');
if (window.Swiper || document.querySelector('.swiper')) libs.push('Swiper');
if (window.Splide || document.querySelector('.splide')) libs.push('Splide');
var autoplay = 0, vids = document.getElementsByTagName('video'); for (var i = 0; i < vids.length; i++) if (vids[i].autoplay) autoplay++;
var running = 0; try { running = document.getAnimations().length; } catch (e) {}
var svgs = 0, sv = document.getElementsByTagName('svg'); for (var i = 0; i < sv.length; i++) { var r = sv[i].getBoundingClientRect(); if (r.width >= 24) svgs++; }
var fonts = {}; try { document.fonts.forEach(function(f){ if (f.status === 'loaded') fonts[f.family.replace(/["']/g, '') + ' ' + f.weight + (f.style !== 'normal' ? ' ' + f.style : '')] = 1; }); } catch (e) {}
var padList = Object.keys(pads).sort(function(a, b){ return pads[b] - pads[a]; }).slice(0, 3);
var btnList = Object.keys(btns).sort(function(a, b){ return btns[b] - btns[a]; }).slice(0, 3);
return JSON.stringify({
  title: document.title, height: Math.round(pageH), fonts: Object.keys(fonts).slice(0, 16), type: type,
  surfaces: top(surf, 6, surfTotal), text: top(txt, 5, txtTotal), accents: top(acc, 5, accTotal),
  layout: {contentWidth: cw, gutter: gutter, sectionPad: padList, buttonRadius: mode(btnR), cardRadius: mode(cardR), buttons: btnList,
    borders: mode(borders), shadows: shadows, stickyHeader: sticky, headerHeight: hh, fullBleed: fullBleed},
  sections: sections,
  motion: {libs: libs, animated: marked, transitions: trans, running: running, autoplay: autoplay, smoothScroll: lenis || getComputedStyle(se).scrollBehavior === 'smooth', markers: Object.keys(markers)},
  media: {img: document.getElementsByTagName('img').length, video: vids.length, svg: svgs, canvas: document.getElementsByTagName('canvas').length, bgimg: bgimgs}
});
`
