//! 绘制托盘图标：图形取自应用图标（盾牌 + Wi-Fi 弧线 + 圆点），叠加状态。
//!
//! macOS 菜单栏用单色模板图，由系统按明暗着色，不能带颜色，所以用形状区分状态；
//! Windows 用彩色图，角标颜色表示状态，同时保留符号，不只靠颜色区分。

#[derive(Clone, Copy, PartialEq, Debug)]
pub enum Status {
    Off,
    On,
    Warn,
    Error,
}

type Rgb = [f32; 3];

const BLACK: Rgb = [0.0, 0.0, 0.0];
const WHITE: Rgb = [1.0, 1.0, 1.0];
const BRAND: Rgb = rgb(0x15, 0x80, 0x3d);
const AMBER: Rgb = rgb(0xf5, 0x9e, 0x0b);
const GRAY: Rgb = rgb(0x8a, 0x8f, 0x96);
const GREEN: Rgb = rgb(0x22, 0xa3, 0x55);
const RED: Rgb = rgb(0xd9, 0x3a, 0x30);

const fn rgb(r: u8, g: u8, b: u8) -> Rgb {
    [r as f32 / 255.0, g as f32 / 255.0, b as f32 / 255.0]
}

// 坐标归一化到 0..1，原点在左上角。
// 盾牌：上沿中间高、两肩低，直边到 SHIELD_WAIST，再收成尖底。
const SHIELD_HALF: f32 = 0.37;
const SHIELD_TOP: f32 = 0.06;
const SHIELD_SHOULDER: f32 = 0.16;
const SHIELD_WAIST: f32 = 0.5;
const SHIELD_TIP: f32 = 0.96;
const STROKE: f32 = 0.085;
// Wi-Fi：圆点和两道弧，圆心在盾牌下部，只取朝上的一段。
const WIFI: (f32, f32) = (0.5, 0.66);
const DOT: f32 = 0.07;
const ARCS: [f32; 2] = [0.165, 0.29];
const ARC_HALF: f32 = 0.042;
const ARC_SPREAD: f32 = 1.05; // |dx| <= -dy * ARC_SPREAD，约 ±46°
// 角标放在右下角，留缝后不碰到弧线和圆点。
const BADGE: (f32, f32) = (0.81, 0.81);
const BADGE_GAP: f32 = 0.235;
const BADGE_RADIUS: f32 = 0.19;
const BADGE_HOLE: f32 = 0.085;

/// 盾牌在高度 y 处的半宽，0 表示不在盾牌里。
fn shield_half(y: f32) -> f32 {
    if y <= SHIELD_WAIST {
        SHIELD_HALF
    } else {
        let t = ((y - SHIELD_WAIST) / (SHIELD_TIP - SHIELD_WAIST)).min(1.0);
        SHIELD_HALF * (1.0 - t * t)
    }
}

/// 盾牌在 x 处的上沿：中间高、两肩低。
fn shield_top(x: f32) -> f32 {
    SHIELD_TOP + (SHIELD_SHOULDER - SHIELD_TOP) * ((x - 0.5).abs() / SHIELD_HALF).min(1.0)
}

/// inset 为向内收的距离，用来画空心的外框。
fn shield(x: f32, y: f32, inset: f32) -> bool {
    let half = shield_half(y) - inset;
    half > 0.0 && (x - 0.5).abs() <= half && y >= shield_top(x) + inset && y <= SHIELD_TIP - inset * 1.6
}

fn circle(x: f32, y: f32, cx: f32, cy: f32, radius: f32) -> bool {
    (x - cx).powi(2) + (y - cy).powi(2) <= radius * radius
}

/// Wi-Fi 图形：圆点和两道弧。
fn wifi(x: f32, y: f32) -> bool {
    let (dx, dy) = (x - WIFI.0, y - WIFI.1);
    if circle(x, y, WIFI.0, WIFI.1, DOT) {
        return true;
    }
    if dy >= 0.0 || dx.abs() > -dy * ARC_SPREAD {
        return false;
    }
    let r = dx.hypot(dy);
    ARCS.iter().any(|a| (r - a).abs() <= ARC_HALF)
}

fn wifi_dot(x: f32, y: f32) -> bool {
    circle(x, y, WIFI.0, WIFI.1, DOT)
}

/// 角标内的符号：Warn 为感叹号，Error 为叉，其余没有符号。
fn badge_glyph(status: Status, x: f32, y: f32) -> bool {
    let (u, v) = (x - BADGE.0, y - BADGE.1);
    match status {
        Status::Warn => (u.abs() <= 0.034 && (-0.125..=0.025).contains(&v)) || circle(u, v, 0.0, 0.09, 0.038),
        Status::Error => {
            let limit = u.abs() <= 0.085 && v.abs() <= 0.085;
            limit && ((u - v).abs() <= 0.05 || (u + v).abs() <= 0.05)
        }
        _ => false,
    }
}

/// macOS 模板图的一个采样点：只有黑色和透明。
fn template_sample(status: Status, x: f32, y: f32) -> Option<Rgb> {
    let has_badge = matches!(status, Status::Warn | Status::Error);
    if has_badge && circle(x, y, BADGE.0, BADGE.1, BADGE_GAP) {
        let filled = circle(x, y, BADGE.0, BADGE.1, BADGE_RADIUS) && !badge_glyph(status, x, y);
        return filled.then_some(BLACK);
    }
    if !shield(x, y, 0.0) {
        return None;
    }
    let filled = if status == Status::Off {
        // 空心表示未运行：只画外框和 Wi-Fi。
        !shield(x, y, STROKE) || wifi(x, y)
    } else {
        // 实心表示运行中：Wi-Fi 从盾牌上镂空。
        !wifi(x, y)
    };
    filled.then_some(BLACK)
}

/// Windows 彩色图的一个采样点：绿色盾牌、白色弧线、琥珀色圆点，与应用图标一致。
fn color_sample(status: Status, x: f32, y: f32) -> Option<Rgb> {
    if circle(x, y, BADGE.0, BADGE.1, BADGE_GAP) {
        if !circle(x, y, BADGE.0, BADGE.1, BADGE_RADIUS) {
            return None;
        }
        // 未运行画成空心环，与实心的运行中在形状上也有区别。
        if status == Status::Off && circle(x, y, BADGE.0, BADGE.1, BADGE_HOLE) {
            return None;
        }
        if badge_glyph(status, x, y) {
            return Some(WHITE);
        }
        return Some(match status {
            Status::Off => GRAY,
            Status::On => GREEN,
            Status::Warn => AMBER,
            Status::Error => RED,
        });
    }
    if !shield(x, y, 0.0) {
        return None;
    }
    if wifi_dot(x, y) {
        return Some(AMBER);
    }
    if wifi(x, y) {
        return Some(WHITE);
    }
    Some(BRAND)
}

/// 每个像素取 4×4 个采样点做抗锯齿。输出非预乘的 RGBA。
fn render(size: usize, sample: impl Fn(f32, f32) -> Option<Rgb>) -> Vec<u8> {
    const SUB: usize = 4;
    let mut out = Vec::with_capacity(size * size * 4);
    for py in 0..size {
        for px in 0..size {
            let (mut sum, mut hits) = ([0.0f32; 3], 0usize);
            for sy in 0..SUB {
                for sx in 0..SUB {
                    let x = (px as f32 + (sx as f32 + 0.5) / SUB as f32) / size as f32;
                    let y = (py as f32 + (sy as f32 + 0.5) / SUB as f32) / size as f32;
                    if let Some(c) = sample(x, y) {
                        hits += 1;
                        for i in 0..3 {
                            sum[i] += c[i];
                        }
                    }
                }
            }
            for channel in sum {
                let value = if hits == 0 { 0.0 } else { channel / hits as f32 };
                out.push((value * 255.0).round() as u8);
            }
            out.push((hits * 255 / (SUB * SUB)) as u8);
        }
    }
    out
}

pub fn template_rgba(status: Status, size: usize) -> Vec<u8> {
    render(size, |x, y| template_sample(status, x, y))
}

pub fn color_rgba(status: Status, size: usize) -> Vec<u8> {
    render(size, |x, y| color_sample(status, x, y))
}

#[cfg(test)]
mod tests {
    use super::*;

    const ALL: [Status; 4] = [Status::Off, Status::On, Status::Warn, Status::Error];

    #[test]
    fn template_is_black_with_alpha_only() {
        for status in ALL {
            let image = template_rgba(status, 36);
            assert_eq!(image.len(), 36 * 36 * 4);
            assert!(image.chunks(4).all(|p| p[..3] == [0, 0, 0]), "{status:?}");
        }
    }

    #[test]
    fn template_states_differ_without_color() {
        for (i, a) in ALL.iter().enumerate() {
            for b in &ALL[i + 1..] {
                assert_ne!(template_rgba(*a, 36), template_rgba(*b, 36), "{a:?} {b:?}");
            }
        }
    }

    #[test]
    fn color_states_differ() {
        for (i, a) in ALL.iter().enumerate() {
            for b in &ALL[i + 1..] {
                assert_ne!(color_rgba(*a, 32), color_rgba(*b, 32), "{a:?} {b:?}");
            }
        }
    }

    #[test]
    fn top_left_corner_is_transparent() {
        for status in ALL {
            assert_eq!(template_rgba(status, 36)[3], 0);
            assert_eq!(color_rgba(status, 32)[3], 0);
        }
    }

    #[test]
    fn badge_never_covers_the_wifi() {
        // 弧线最靠近角标的是外弧的右端点。
        let angle = ARC_SPREAD.atan();
        let r = ARCS[1] + ARC_HALF;
        let end = (WIFI.0 + r * angle.sin(), WIFI.1 - r * angle.cos());
        let distance = (BADGE.0 - end.0).hypot(BADGE.1 - end.1);
        assert!(distance >= BADGE_GAP, "{distance}");
        let dot = (BADGE.0 - WIFI.0).hypot(BADGE.1 - WIFI.1) - DOT;
        assert!(dot >= BADGE_GAP, "{dot}");
        assert!(BADGE.0 + BADGE_RADIUS <= 1.0);
    }

    #[test]
    fn wifi_sits_inside_the_shield() {
        let r = ARCS[1] + ARC_HALF;
        assert!(shield(WIFI.0, WIFI.1 - r, STROKE), "外弧顶部应在盾牌内");
        assert!(shield(WIFI.0, WIFI.1 + DOT, STROKE), "圆点应在盾牌内");
    }

    #[test]
    fn stopped_icon_is_hollow() {
        let opaque = |status| template_rgba(status, 36).chunks(4).filter(|p| p[3] > 127).count();
        assert!(opaque(Status::Off) < opaque(Status::On));
    }
}
