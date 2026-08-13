import { useEffect, useRef } from 'react'

// The memcode brand wordmark: MEMCODE rendered in matrix/katakana digital-rain
// glyphs clipped to the letter shapes, violet phosphor over sparse rain. Ported
// faithfully from the website's MatrixGlyphWordmark (apps/www) and the CLI's
// banner.go so the desktop shares the one brand treatment.

const GLYPHS = 'ハケモサナアウキニツヲヤカコレミネテヒイオリ0123456789=<>+*|/\\'.split('')
const LIT = ['#e0d8ff', '#d2c6ff', '#c4b6ff', '#b9aaff']

const pickGlyph = () => GLYPHS[(Math.random() * GLYPHS.length) | 0]
const pickLit = () => LIT[(Math.random() * LIT.length) | 0]

interface BaseCell {
  x: number
  y: number
  glyph: string
  color: string
}
interface Column {
  x: number
  y: number
  speed: number
  len: number
}

export function MatrixWordmark({ word = 'MEMCODE', className = '' }: { word?: string; className?: string }) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const wordRef = useRef(word)
  wordRef.current = word

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    let raf = 0
    let running = true

    const baseCell = 11
    let baseCells: BaseCell[] = []
    let lastW = 0
    let lastH = 0
    const fc = 14
    let columns: Column[] = []

    function setup(w: number, h: number) {
      lastW = w
      lastH = h
      baseCells = []
      for (let y = baseCell / 2; y < h; y += baseCell) {
        for (let x = baseCell / 2; x < w; x += baseCell) {
          baseCells.push({ x, y, glyph: pickGlyph(), color: pickLit() })
        }
      }
      const cell = w < 640 ? 10 : fc
      const cols = Math.ceil(w / cell)
      columns = []
      for (let i = 0; i < cols; i++) {
        columns.push({ x: i * cell + cell / 2, y: Math.random() * h, speed: 0.15 + Math.random() * 0.3, len: 4 + ((Math.random() * 8) | 0) })
      }
    }

    function draw() {
      if (!canvas || !ctx) return
      const cssW = canvas.clientWidth
      const cssH = canvas.clientHeight
      if (cssW === 0 || cssH === 0) return
      if (cssW !== lastW || cssH !== lastH) setup(cssW, cssH)

      const dpr = window.devicePixelRatio || 1
      if (canvas.width !== cssW * dpr || canvas.height !== cssH * dpr) {
        canvas.width = cssW * dpr
        canvas.height = cssH * dpr
      }

      const lay = document.createElement('canvas')
      lay.width = cssW * dpr
      lay.height = cssH * dpr
      const lx = lay.getContext('2d')
      if (!lx) return
      lx.setTransform(dpr, 0, 0, dpr, 0, 0)
      lx.clearRect(0, 0, cssW, cssH)
      lx.textAlign = 'center'
      lx.textBaseline = 'middle'

      // Static dim base — keeps MEMCODE readable.
      lx.font = `700 ${baseCell * 1.05}px 'SF Mono',Menlo,Monaco,monospace`
      for (const c of baseCells) {
        lx.globalAlpha = 0.55
        lx.fillStyle = c.color
        lx.fillText(c.glyph, c.x, c.y)
      }

      // Rain sweeping through.
      const cell = cssW < 640 ? 10 : fc
      const alpha = cssW < 640 ? 0.27 : 0.19
      lx.font = `400 ${cell * 0.86}px 'SF Mono',Menlo,Monaco,monospace`
      for (const col of columns) {
        for (let j = 0; j < col.len; j++) {
          const y = col.y - j * cell
          if (y < -cell || y > cssH + cell) continue
          const tail = j / col.len
          lx.globalAlpha = (1 - tail) * (alpha + Math.random() * 0.13)
          lx.fillStyle = '#6a5da6'
          lx.fillText(pickGlyph(), col.x, y)
        }
        col.y += col.speed
        if (col.y - col.len * cell > cssH) {
          col.y = -Math.random() * cssH * 0.5
          col.speed = 0.15 + Math.random() * 0.3
          col.len = 4 + ((Math.random() * 8) | 0)
        }
      }
      lx.globalAlpha = 1

      // Clip both layers to the word shape.
      lx.globalCompositeOperation = 'destination-in'
      const w = (wordRef.current || 'MEMCODE').toUpperCase()
      let fs = cssH * 0.92
      lx.font = `900 ${fs}px 'Arial Black',Arial,sans-serif`
      while (lx.measureText(w).width > cssW * 0.94 && fs > 6) {
        fs -= 1
        lx.font = `900 ${fs}px 'Arial Black',Arial,sans-serif`
      }
      lx.fillStyle = '#fff'
      lx.fillText(w, cssW / 2, cssH / 2)

      ctx.setTransform(1, 0, 0, 1, 0, 0)
      ctx.clearRect(0, 0, canvas.width, canvas.height)
      ctx.drawImage(lay, 0, 0)
    }

    function loop() {
      if (!running) return
      draw()
      raf = requestAnimationFrame(loop)
    }
    loop()

    const ro = new ResizeObserver(() => {
      lastW = 0
    })
    ro.observe(canvas)

    return () => {
      running = false
      cancelAnimationFrame(raf)
      ro.disconnect()
    }
  }, [])

  return <canvas ref={canvasRef} className={className} aria-label={word} />
}
