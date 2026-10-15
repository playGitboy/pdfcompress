#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 pdf.png 转成 Windows 图标 app.ico（多尺寸）。

纯 Python 实现，不依赖 Pillow 等第三方库：
  - 自己解码 PNG（zlib 解压 + 5 种滤波还原）
  - 用面积平均法（box filter）缩放，缩小画质优于最近邻
  - 按 ICO 规范打包 16/24/32/48/64/128/256 七种尺寸

用法:
    python png2ico.py <输入.png> <输出.ico>
"""

import struct
import sys
import zlib

# ---------------------------------------------------------------- PNG 解码

PNG_SIG = b"\x89PNG\r\n\x1a\n"


def _paeth(a, b, c):
    p = a + b - c
    pa = abs(p - a)
    pb = abs(p - b)
    pc = abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    if pb <= pc:
        return b
    return c


def decode_png(data):
    """解码 PNG，返回 (width, height, rgba_bytes)。

    支持位深 8，颜色类型 0/2/4/6（灰度、RGB、灰度+A、RGBA），
    非隔行扫描。这是绝大多数图标 PNG 的形态。
    """
    if data[:8] != PNG_SIG:
        raise ValueError("不是 PNG 文件")

    pos = 8
    idat = bytearray()
    width = height = bit_depth = color_type = interlace = None
    palette = None
    trns = None

    while pos < len(data):
        (length,) = struct.unpack_from(">I", data, pos)
        ctype = data[pos + 4:pos + 8]
        chunk = data[pos + 8:pos + 8 + length]
        pos += 12 + length

        if ctype == b"IHDR":
            (width, height, bit_depth, color_type,
             _comp, _filt, interlace) = struct.unpack(">IIBBBBB", chunk)
        elif ctype == b"PLTE":
            palette = [tuple(chunk[i:i + 3]) for i in range(0, len(chunk), 3)]
        elif ctype == b"tRNS":
            trns = chunk
        elif ctype == b"IDAT":
            idat += chunk
        elif ctype == b"IEND":
            break

    if width is None:
        raise ValueError("PNG 缺少 IHDR")
    if bit_depth != 8:
        raise ValueError("暂不支持位深 %d（仅支持 8）" % bit_depth)
    if interlace:
        raise ValueError("暂不支持隔行扫描 PNG")

    raw = zlib.decompress(bytes(idat))

    # 每像素字节数与通道数
    channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[color_type]
    bpp = channels
    stride = width * bpp

    # ---- 逐行反滤波
    out = bytearray(height * stride)
    prev = bytearray(stride)
    rp = 0
    for y in range(height):
        ft = raw[rp]
        rp += 1
        line = bytearray(raw[rp:rp + stride])
        rp += stride
        if ft == 0:
            pass
        elif ft == 1:   # Sub
            for i in range(bpp, stride):
                line[i] = (line[i] + line[i - bpp]) & 0xFF
        elif ft == 2:   # Up
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ft == 3:   # Average
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif ft == 4:   # Paeth
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                c = prev[i - bpp] if i >= bpp else 0
                line[i] = (line[i] + _paeth(a, prev[i], c)) & 0xFF
        else:
            raise ValueError("未知滤波类型 %d" % ft)
        out[y * stride:(y + 1) * stride] = line
        prev = line

    # ---- 转成 RGBA
    rgba = bytearray(width * height * 4)
    if color_type == 6:       # RGBA
        rgba[:] = out
    elif color_type == 2:     # RGB
        for i in range(width * height):
            rgba[i * 4] = out[i * 3]
            rgba[i * 4 + 1] = out[i * 3 + 1]
            rgba[i * 4 + 2] = out[i * 3 + 2]
            rgba[i * 4 + 3] = 255
    elif color_type == 0:     # 灰度
        for i in range(width * height):
            v = out[i]
            rgba[i * 4] = rgba[i * 4 + 1] = rgba[i * 4 + 2] = v
            rgba[i * 4 + 3] = 255
    elif color_type == 4:     # 灰度 + A
        for i in range(width * height):
            v = out[i * 2]
            rgba[i * 4] = rgba[i * 4 + 1] = rgba[i * 4 + 2] = v
            rgba[i * 4 + 3] = out[i * 2 + 1]
    elif color_type == 3:     # 调色板
        for i in range(width * height):
            idx = out[i]
            if idx >= len(palette):
                continue
            r, g, b = palette[idx]
            rgba[i * 4] = r
            rgba[i * 4 + 1] = g
            rgba[i * 4 + 2] = b
            if trns is not None and idx < len(trns):
                rgba[i * 4 + 3] = trns[idx]
            else:
                rgba[i * 4 + 3] = 255

    return width, height, bytes(rgba)


# ---------------------------------------------------------------- 缩放

def resize_rgba(src, sw, sh, dw, dh):
    """面积平均缩放（box filter），带 alpha 预乘以正确处理边缘。

    缩小：把源区域按面积权重平均；放大：双线性（此处图标只缩小，够用）。
    """
    dst = bytearray(dw * dh * 4)
    for dy in range(dh):
        # 该目标行对应的源区间
        sy0 = dy * sh / dh
        sy1 = (dy + 1) * sh / dh
        iy0 = int(sy0)
        iy1 = min(sh, int(sy1) + 1)
        for dx in range(dw):
            sx0 = dx * sw / dw
            sx1 = (dx + 1) * sw / dw
            ix0 = int(sx0)
            ix1 = min(sw, int(sx1) + 1)

            acc_r = acc_g = acc_b = acc_a = 0.0
            wsum = 0.0
            for sy in range(iy0, iy1):
                # 纵向覆盖权重
                wy = min(sy1, sy + 1) - max(sy0, sy)
                if wy <= 0:
                    continue
                for sx in range(ix0, ix1):
                    wx = min(sx1, sx + 1) - max(sx0, sx)
                    if wx <= 0:
                        continue
                    w = wx * wy
                    i = (sy * sw + sx) * 4
                    a = src[i + 3] / 255.0
                    # 预乘 alpha 后加权，避免透明边缘发黑
                    acc_r += src[i] * a * w
                    acc_g += src[i + 1] * a * w
                    acc_b += src[i + 2] * a * w
                    acc_a += a * w
                    wsum += w

            o = (dy * dw + dx) * 4
            if wsum <= 0 or acc_a <= 0:
                dst[o:o + 4] = b"\x00\x00\x00\x00"
                continue
            dst[o] = min(255, int(round(acc_r / acc_a)))
            dst[o + 1] = min(255, int(round(acc_g / acc_a)))
            dst[o + 2] = min(255, int(round(acc_b / acc_a)))
            dst[o + 3] = min(255, int(round(acc_a / wsum * 255)))
    return bytes(dst)


# ---------------------------------------------------------------- ICO 打包

def pack_ico(images):
    """images: [(size, rgba_bytes)] -> ICO 字节。"""
    count = len(images)
    header = struct.pack("<HHH", 0, 1, count)
    entries = b""
    blobs = b""
    offset = 6 + 16 * count

    for size, rgba in images:
        w = h = size
        # BMP 形式：BITMAPINFOHEADER + BGRA 像素（自下而上）+ AND 掩码
        pixels = bytearray()
        for y in range(h - 1, -1, -1):
            row = rgba[y * w * 4:(y + 1) * w * 4]
            for x in range(w):
                i = x * 4
                pixels += bytes((row[i + 2], row[i + 1], row[i], row[i + 3]))

        row_bytes = ((w + 31) // 32) * 4
        mask = bytearray()
        for y in range(h - 1, -1, -1):
            mrow = bytearray(row_bytes)
            for x in range(w):
                if rgba[(y * w + x) * 4 + 3] < 128:
                    mrow[x // 8] |= (0x80 >> (x % 8))
            mask += mrow

        dib = struct.pack(
            "<IiiHHIIiiII",
            40, w, h * 2, 1, 32, 0,
            len(pixels) + len(mask), 0, 0, 0, 0,
        ) + bytes(pixels) + bytes(mask)

        entries += struct.pack(
            "<BBBBHHII",
            w if w < 256 else 0,
            h if h < 256 else 0,
            0, 0, 1, 32,
            len(dib), offset,
        )
        blobs += dib
        offset += len(dib)

    return header + entries + blobs


# ---------------------------------------------------------------- 主流程

def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 1

    src_path, out_path = sys.argv[1], sys.argv[2]
    with open(src_path, "rb") as f:
        png = f.read()

    sw, sh, rgba = decode_png(png)
    print("源图: %dx%d RGBA (%d 字节)" % (sw, sh, len(png)))

    # 目标尺寸：Windows 常用档位，全部 <= 源尺寸以保证缩小质量
    wanted = [16, 24, 32, 48, 64, 128, 256]
    images = []
    for s in wanted:
        if s > max(sw, sh):
            continue
        if s == sw and sh == s:
            scaled = rgba
        else:
            scaled = resize_rgba(rgba, sw, sh, s, s)
        images.append((s, scaled))
        print("  缩放 %dx%d" % (s, s))

    data = pack_ico(images)
    with open(out_path, "wb") as f:
        f.write(data)
    print("已生成: %s (%d 字节, %d 个尺寸)" % (out_path, len(data), len(images)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
