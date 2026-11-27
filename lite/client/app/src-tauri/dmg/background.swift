// 生成 dmg 窗口背景（660×424，带 2 倍图）：上半部分「拖到应用程序」，下半部分「提示已损坏时怎么办」。
// 布局按 660×420 画，贴着顶部；底下多 4px 同色填充：build-app.sh 的窗口高度含标题栏（新版 macOS 32px、旧版 28px），
// 内容区是 420 或 424，两种情况下背景都铺满，不露出白边。
// 图标位置与 scripts/build-app.sh 里 bundle_dmg.sh 的 --icon / --app-drop-link 一致，改布局时两边一起改。
// 用法（在本目录）：swift background.swift . && tiffutil -cathidpicheck background.png background@2x.png -out background.tiff && rm background*.png
// 仓库里只放合成好的 background.tiff（Finder 按屏幕选 1 倍或 2 倍图）。
import AppKit

let width: CGFloat = 660, height: CGFloat = 420 // 布局坐标
let canvasHeight: CGFloat = 424 // 实际图片高度
let out = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "."

func font(_ size: CGFloat, _ weight: NSFont.Weight) -> NSFont {
    // 苹方是系统自带的中文字体；拿不到时退回系统字体。
    let name = weight == .regular ? "PingFangSC-Regular" : "PingFangSC-Semibold"
    return NSFont(name: name, size: size) ?? NSFont.systemFont(ofSize: size, weight: weight)
}

func text(_ s: String, _ f: NSFont, _ color: NSColor, x: CGFloat, y: CGFloat, w: CGFloat, align: NSTextAlignment = .center) {
    let p = NSMutableParagraphStyle()
    p.alignment = align
    p.lineSpacing = 3
    let attrs: [NSAttributedString.Key: Any] = [.font: f, .foregroundColor: color, .paragraphStyle: p]
    let str = NSAttributedString(string: s, attributes: attrs)
    let h = str.boundingRect(with: NSSize(width: w, height: 400), options: .usesLineFragmentOrigin).height
    // 坐标从上往下算，方便对照 Finder 里的图标位置。
    str.draw(with: NSRect(x: x, y: height - y - h, width: w, height: h), options: .usesLineFragmentOrigin)
}

func render(scale: CGFloat, to path: String) {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(width * scale), pixelsHigh: Int(canvasHeight * scale),
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    rep.size = NSSize(width: width, height: canvasHeight)
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)

    let ink = NSColor(srgbRed: 0.13, green: 0.15, blue: 0.19, alpha: 1)
    let muted = NSColor(srgbRed: 0.40, green: 0.43, blue: 0.49, alpha: 1)
    let accent = NSColor(srgbRed: 0.85, green: 0.45, blue: 0.10, alpha: 1)

    NSColor(srgbRed: 0.97, green: 0.97, blue: 0.98, alpha: 1).setFill()
    NSRect(x: 0, y: 0, width: width, height: canvasHeight).fill()
    // 布局贴着顶部：坐标系上移多出来的高度。
    let shift = NSAffineTransform()
    shift.translateX(by: 0, yBy: canvasHeight - height)
    shift.concat()

    // 上半部分：拖动安装。图标中心在 (165, 150) 和 (495, 150)。
    text("把 Banbu Connect Lite 拖到「应用程序」", font(17, .semibold), ink, x: 0, y: 30, w: width)
    let arrow = NSBezierPath()
    arrow.lineWidth = 3
    arrow.lineCapStyle = .round
    arrow.lineJoinStyle = .round
    arrow.move(to: NSPoint(x: 268, y: height - 150))
    arrow.line(to: NSPoint(x: 392, y: height - 150))
    arrow.move(to: NSPoint(x: 378, y: height - 140))
    arrow.line(to: NSPoint(x: 392, y: height - 150))
    arrow.line(to: NSPoint(x: 378, y: height - 160))
    NSColor(srgbRed: 0.70, green: 0.72, blue: 0.76, alpha: 1).setStroke()
    arrow.stroke()

    // 下半部分：提示框。左侧是「修复无法打开」图标（中心 (110, 330)），右侧是「安装说明」（中心 (550, 330)）。
    let box = NSBezierPath(roundedRect: NSRect(x: 24, y: 24, width: width - 48, height: 132), xRadius: 10, yRadius: 10)
    NSColor(srgbRed: 1.0, green: 0.96, blue: 0.90, alpha: 1).setFill()
    box.fill()
    NSColor(srgbRed: 0.98, green: 0.84, blue: 0.62, alpha: 1).setStroke()
    box.lineWidth = 1
    box.stroke()
    text("打开时提示「已损坏，无法打开」？", font(14, .semibold), accent, x: 150, y: 284, w: 360)
    text("完成上面的拖动后，双击左边的「修复无法打开」。\n若提示无法验证：系统设置 → 隐私与安全性 → 仍要打开。\n也可以打开右边的「安装说明」，在终端执行一行命令。",
         font(12, .regular), muted, x: 150, y: 310, w: 360)

    NSGraphicsContext.restoreGraphicsState()
    try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
}

render(scale: 1, to: "\(out)/background.png")
render(scale: 2, to: "\(out)/background@2x.png")
