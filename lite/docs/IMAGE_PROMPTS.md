# 图片素材

配色见 ADR-013：主色罗兰紫 `#6D28D9`，辅色琥珀 `#F59E0B`。

| 图 | 用在 | 状态 |
|---|---|---|
| 应用图标 | 窗口、任务栏、安装包 | 已使用方案 A：用户生成后清理透明通道和杂点，居中并留 12% 边距，存为 `client/app/src-tauri/app-icon.png` |
| 托盘图标 | 系统托盘、菜单栏 | 不需要生成：由 `client/app/src-tauri/src/tray_icon.rs` 绘制，图形按应用图标方案 A。macOS 用单色模板图，以空心、实心和角标符号表示状态；Windows 用彩色图和状态角标 |

---

## 应用图标

| 项 | 要求 |
|---|---|
| 尺寸 | 1024×1024 PNG，透明背景，主体四周留约 12% 边距 |
| 存放 | `client/app/src-tauri/app-icon.png`，构建时由 `tauri icon` 导出 `.ico`、`.icns` 和各尺寸 PNG |
| 检查 | 缩小到 32×32 仍能看清主体，在浅色和深色任务栏上都清晰 |

### 方案 A：路由盒（推荐）

盒子代表路由盒，线路代表流量，琥珀圆点代表保护流量的闸门。

**提示词（中文）**

> 扁平化应用图标，正面视角的圆角方形盒子，整体为实心罗兰紫 #6D28D9。盒子中央有一条粗白线，从左边缘水平穿入，从右边缘穿出；白线正中是一个琥珀色 #F59E0B 的实心圆点，圆点外有一圈白色细缝与白线隔开。几何、简洁、居中对称，无文字、无渐变、无阴影、无高光、无立体效果。透明背景，主体四周留 12% 边距，缩小到 32 像素仍清晰可辨。

**Prompt (English)**

> Flat app icon, a rounded square box seen straight on, solid violet #6D28D9. A thick white line enters from the left edge, runs horizontally through the center, and exits at the right edge. At the exact center of the line sits a solid amber #F59E0B circle, separated from the line by a thin white ring. Geometric, minimal, centered and symmetrical. No text, no gradient, no shadow, no highlight, no 3D effect. Transparent background, 12% margin on all sides, must stay legible at 32 px.

### 方案 B：保护开关

更突出 Kill Switch，但开关图形较常见，辨识度略低。

**提示词（中文）**

> 扁平化应用图标，正面视角的圆角方形盒子，整体为实心罗兰紫 #6D28D9。盒子中央是一个横向的白色胶囊形拨动开关，开关滑块为琥珀色 #F59E0B 的实心圆，位于右侧开启位置。几何、简洁、居中，无文字、无渐变、无阴影、无高光、无立体效果。透明背景，主体四周留 12% 边距，缩小到 32 像素仍清晰可辨。

**Prompt (English)**

> Flat app icon, a rounded square box seen straight on, solid violet #6D28D9. In the center is a horizontal white pill-shaped toggle switch with a solid amber #F59E0B circular knob in the right, switched-on position. Geometric, minimal, centered. No text, no gradient, no shadow, no highlight, no 3D effect. Transparent background, 12% margin on all sides, must stay legible at 32 px.
