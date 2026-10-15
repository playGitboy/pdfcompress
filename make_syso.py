#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""为 Windows 版 PDFCompress 生成资源文件 (.syso)。

Go 在构建 windows/386 目标时，会自动链接包目录下的 *.syso 文件。
因此只要生成 winres.syso 放在 main.go 同级，即可嵌入：
  - 应用程序图标（资源类型 RT_ICON=3 / RT_GROUP_ICON=14）
  - 版本信息   （资源类型 RT_VERSION=16，即属性页里的"详细信息"）

本脚本**不依赖任何外部工具**（不需要 windres / mingw / go-winres），
纯 Python 直接构造 COFF 目标文件，符合本项目"离线可构建"的约束。

用法:
    python make_syso.py <输出.syso> <图标.ico> [版本号]

COFF 文件布局（单节 .rsrc）:
    [COFF Header 20B]
    [Section Header 40B]
    [节原始数据：资源目录 + 目录项 + 数据项 + 资源内容]
    [符号表：1 个符号 .rsrc]
    [字符串表：4 字节长度 + 名称]
"""

import io
import os
import struct
import sys

# ---------------------------------------------------------------- 常量

IMAGE_FILE_MACHINE_I386 = 0x014C
IMAGE_SCN_CNT_INITIALIZED_DATA = 0x00000040
IMAGE_SCN_MEM_READ = 0x40000000
IMAGE_SCN_MEM_WRITE = 0x80000000

# 资源类型
RT_ICON = 3
RT_GROUP_ICON = 14
RT_VERSION = 16

# 语言：中文简体
LANG_ZH_CN = 0x0804


# ---------------------------------------------------------------- 小工具

def align(n, a=4):
    """向上对齐到 a 的整数倍。"""
    return (n + a - 1) // a * a


def wstr(s):
    """UTF-16LE 编码（不带 BOM）。"""
    return s.encode("utf-16-le")


def parse_ico(data):
    """解析 .ico 文件，返回 (header, [条目]), 每个条目含 w/h/colors/planes/bpp/size/offset/data。

    支持 ICO 与 CUR（本项目只用 ICO）。
    """
    if len(data) < 6:
        raise ValueError("ICO 文件过小")
    reserved, ico_type, count = struct.unpack_from("<HHH", data, 0)
    if reserved != 0:
        raise ValueError("不是合法的 ICO 文件（reserved != 0）")
    if ico_type != 1:
        raise ValueError("不是 ICO 图标（type=%d，期望 1）" % ico_type)
    if count == 0:
        raise ValueError("ICO 中没有任何图像")

    entries = []
    off = 6
    for _ in range(count):
        (w, h, colors, _res, planes, bpp, size, offset) = struct.unpack_from(
            "<BBBBHHII", data, off
        )
        entries.append({
            "width": w if w != 0 else 256,
            "height": h if h != 0 else 256,
            "colors": colors,
            "planes": planes,
            "bpp": bpp,
            "size": size,
            "offset": offset,
            "data": data[offset:offset + size],
        })
        off += 16
    return entries


def build_group_icon(entries, icon_id_base):
    """构造 RT_GROUP_ICON 的内容（GRPICONDIR + 各 GRPICONDIRENTRY）。"""
    out = io.BytesIO()
    # GRPICONDIR: reserved(2) type(2) count(2)
    out.write(struct.pack("<HHH", 0, 1, len(entries)))
    for i, e in enumerate(entries):
        out.write(struct.pack(
            "<BBBBHHIH",
            e["width"] if e["width"] < 256 else 0,
            e["height"] if e["height"] < 256 else 0,
            e["colors"],
            0,                      # reserved
            e["planes"] or 1,
            e["bpp"] or 32,
            e["size"],
            icon_id_base + i,       # 指向 RT_ICON 的资源 ID
        ))
    return out.getvalue()


def vs_align(n):
    """版本资源内部按 4 字节对齐。"""
    return align(n, 4)


def vs_node(key, value=b"", children=b"", value_is_words=False,
            value_is_binary=False):
    """构造 VS_VERSIONINFO 的一个节点。

    节点布局（按 Win32 规范）:
        wLength(2) wValueLength(2) wType(2) szKey(UTF-16 含 \\0)
        Padding1 (对齐到 4 字节)
        Value
        Padding2 (对齐到 4 字节)
        Children

    参数:
        key              节点名（如 "StringFileInfo"）
        value            值区字节（可为空）
        children         子节点拼接后的字节（可为空）
        value_is_words   值为字符串时 wValueLength 以"字"计
        value_is_binary  值为二进制块（VS_FIXEDFILEINFO）时 wType 必须为 0。
                         ⚠ 根节点的 wType 必须为 0，否则 Windows 会把
                         52 字节的 VS_FIXEDFILEINFO 当字符串处理，
                         属性页会显示成乱码。字符串节点才用 wType=1。
    """
    key_b = wstr(key) + b"\x00\x00"
    header_len = 6 + len(key_b)
    pad1 = align(header_len, 4) - header_len

    if not value:
        vlen = 0
        wtype = 0
    else:
        vlen = (len(value) // 2) if value_is_words else len(value)
        wtype = 0 if value_is_binary else 1

    # 子节点起点 = align(Value 起点 + len(Value), 4)，
    # 等价于 header_len + pad1 + align(len(value), 4)
    value_span = align(len(value), 4)
    total = header_len + pad1 + value_span + len(children)

    buf = bytearray()
    buf += struct.pack("<HHH", total, vlen, wtype)
    buf += key_b
    buf += b"\x00" * pad1
    buf += value
    buf += b"\x00" * (value_span - len(value))
    buf += children
    return bytes(buf)


def build_fixed_file_info(file_version, product_version, flags=0):
    """VS_FIXEDFILEINFO（52 字节），版本号以 MS<<16|LS 表示。"""
    fv = (file_version[0] << 16) | file_version[1]
    fr = (file_version[2] << 16) | file_version[3]
    pv = (product_version[0] << 16) | product_version[1]
    pr = (product_version[2] << 16) | product_version[3]
    return struct.pack(
        "<IIIIIIIIIIIII",
        0xFEEF04BD,   # dwSignature
        0x00010000,   # dwStrucVersion (1.0)
        fv,           # dwFileVersionMS
        fr,           # dwFileVersionLS
        pv,           # dwProductVersionMS
        pr,           # dwProductVersionLS
        0x3F,         # dwFileFlagsMask
        flags,        # dwFileFlags
        0x00040004,   # dwFileOS = VOS_NT_WINDOWS32
        0x00000001,   # dwFileType = VFT_APP
        0x00000000,   # dwFileSubtype
        0x00000000,   # dwFileDateMS
        0x00000000,   # dwFileDateLS
    )


def build_version_info(info):
    """构造完整的 VS_VERSIONINFO 资源内容。

    层级结构:
        VS_VERSION_INFO
          ├─ VS_FIXEDFILEINFO (值)
          ├─ StringFileInfo
          │    └─ 080404B0 (语言+代码页)
          │         ├─ CompanyName / FileDescription / ... (各字符串节点)
          └─ VarFileInfo
               └─ Translation
    """
    # 各字符串字段的顺序按惯例排列
    keys = [
        ("CompanyName", info["company"]),
        ("FileDescription", info["description"]),
        ("FileVersion", info["file_version_str"]),
        ("InternalName", info["internal_name"]),
        ("LegalCopyright", info["copyright"]),
        ("OriginalFilename", info["original_filename"]),
        ("ProductName", info["product_name"]),
        ("ProductVersion", info["product_version_str"]),
        ("Comments", info["comments"]),
    ]

    # String 节点：值为 UTF-16 字符串（含结束符），wValueLength 以"字"计
    string_nodes = b""
    for k, v in keys:
        if not v:
            continue
        string_nodes += vs_node(k, wstr(v) + b"\x00\x00", value_is_words=True)

    # StringTable: 键名 "080404B0" = 语言 0x0804(简中) + 代码页 0x04B0(1200/UTF-16)
    string_table = vs_node("080404B0", children=string_nodes)
    string_file_info = vs_node("StringFileInfo", children=string_table)

    # VarFileInfo -> Translation
    translation_val = struct.pack("<HH", LANG_ZH_CN, 1200)
    var_file_info = vs_node(
        "VarFileInfo",
        children=vs_node("Translation", translation_val, value_is_words=True),
    )

    fixed = build_fixed_file_info(info["file_version"], info["product_version"])

    # 根节点：值区是 VS_FIXEDFILEINFO（二进制，wType=0，wValueLength=52 字节）
    root = vs_node(
        "VS_VERSION_INFO",
        value=fixed,
        children=string_file_info + var_file_info,
        value_is_binary=True,
    )
    return root


# ---------------------------------------------------------------- 资源树构造

def build_rsrc_section(entries_by_type):
    """构造整个 .rsrc 节数据。

    entries_by_type: {type_id: [(name_id, lang_id, data_bytes), ...]}

    资源目录结构（三层）:
        Type -> Name -> Language -> DataEntry(指向数据)
    所有目录/数据项按 (ID, 语言) 排序，Windows 要求有序，否则 FindResource 可能失败。
    """
    # 先规划：一个根目录 + 每个 type 一个二级目录 + 每个 name 一个三级目录
    # 目录项 = 8 字节(ID 4 + Offset 4)；数据项 = 16 字节
    # 偏移高位为 1 表示指向子目录

    # 计算各层需要的空间
    types = sorted(entries_by_type.keys())
    num_root = len(types)

    # 我们先为每个 type 建立名字列表
    names_per_type = {t: sorted([e[0] for e in entries_by_type[t]]) for t in types}

    # 布局规划
    pos_root_dir = 0
    pos_root_entries = pos_root_dir + 16
    cur = pos_root_entries + num_root * 8

    level2_dirs = {}    # type -> offset
    level2_entries = {}  # type -> offset
    for t in types:
        level2_dirs[t] = vs_align(cur)
        cur = level2_dirs[t] + 16
        level2_entries[t] = vs_align(cur)
        cur = level2_entries[t] + len(names_per_type[t]) * 8

    # 三级目录：每个 (type, name)
    level3_dirs = {}
    level3_entries = {}
    for t in types:
        for n in names_per_type[t]:
            level3_dirs[(t, n)] = vs_align(cur)
            cur = level3_dirs[(t, n)] + 16
            level3_entries[(t, n)] = vs_align(cur)
            cur = level3_entries[(t, n)] + 8  # 单个语言项

    # 数据项区
    data_entry_pos = {}
    for t in types:
        for n in names_per_type[t]:
            data_entry_pos[(t, n)] = vs_align(cur)
            cur = data_entry_pos[(t, n)] + 16

    pos_data = vs_align(cur)

    # 现在开始实际写入
    buf = bytearray()

    def put(offset, data):
        if len(buf) < offset + len(data):
            buf.extend(b"\x00" * (offset + len(data) - len(buf)))
        buf[offset:offset + len(data)] = data

    # ---- 数据区（内容 + 4 字节对齐）
    # entries_by_type: {type_id: [(name_id, lang_id, content_bytes), ...]}
    data_blob = bytearray()
    data_offsets = {}   # (t,n) -> (文件内偏移, 长度)
    for t in types:
        for (nt, nl, content) in entries_by_type[t]:
            if nt not in names_per_type[t]:
                continue
            off = pos_data + len(data_blob)
            data_offsets[(t, nt)] = (off, len(content))
            data_blob.extend(content)
            pad = vs_align(len(data_blob)) - len(data_blob)
            data_blob.extend(b"\x00" * pad)

    # ---- 根目录
    put(pos_root_dir, struct.pack("<IIHHHH", 0, 0, 0, 0, 0, num_root))
    for i, t in enumerate(types):
        put(pos_root_entries + i * 8,
            struct.pack("<II", t, 0x80000000 | level2_dirs[t]))

    # ---- 二级目录（每个 type）
    for t in types:
        ns = names_per_type[t]
        put(level2_dirs[t], struct.pack("<IIHHHH", 0, 0, 0, 0, 0, len(ns)))
        for i, n in enumerate(ns):
            put(level2_entries[t] + i * 8,
                struct.pack("<II", n, 0x80000000 | level3_dirs[(t, n)]))

    # ---- 三级目录（每个 name，单语言）
    # 目录头格式: Characteristics(4) TimeDateStamp(4) MajorVersion(2)
    #             MinorVersion(2) NumberOfNamedEntries(2) NumberOfIdEntries(2)
    # 语言 ID (0x0804) 既非字符串也非序号 0，因此必须计入 **NumberOfIdEntries**；
    # 若误计入 NumberOfNamedEntries，Windows 会按字符串名去取，
    # 结果 FindResource 找不到该语言分支，属性页中的"详细信息"就全部为空。
    for t in types:
        for n in names_per_type[t]:
            put(level3_dirs[(t, n)], struct.pack("<IIHHHH", 0, 0, 0, 0, 1, 0))
            put(level3_entries[(t, n)],
                struct.pack("<II", LANG_ZH_CN, data_entry_pos[(t, n)]))

    # ---- 数据项
    # 注意：数据项的 RVA 字段必须记录位置，稍后生成重定位项，
    # 由链接器在链接时加上 .rsrc 节的最终 VA（否则资源无法被定位）。
    rva_fixups = []
    for t in types:
        for n in names_per_type[t]:
            off, ln = data_offsets[(t, n)]
            put(data_entry_pos[(t, n)], struct.pack("<IIII", off, ln, 0, 0))
            # 记录 RVA 字段（数据项前 4 字节）在节内的偏移
            rva_fixups.append(data_entry_pos[(t, n)])

    # 合并数据区
    if len(buf) < pos_data:
        buf.extend(b"\x00" * (pos_data - len(buf)))
    buf.extend(data_blob)

    return bytes(buf), rva_fixups


# ---------------------------------------------------------------- COFF 封装

# IMAGE_REL_I386_DIR32NB：32 位无基址相对地址（写入节 VA 偏移，不含镜像基址）
IMAGE_REL_I386_DIR32NB = 0x0007


def build_coff(section_data, rva_fixups, section_name=b".rsrc"):
    """把 COFF 目标文件封装出来。

    布局:
        [COFF Header 20B]
        [Section Header 40B]
        [节原始数据 (section_data)]
        [重定位表 (每条 10B)]
        [符号表: 主符号 18B + 辅助符号 18B = 36B]
        [字符串表: 4B 长度 + ".rsrc\\0"]

    为什么需要重定位表:
        资源目录里「数据项」的首字段是数据的 RVA。我们在生成 .syso 时
        并不知道 .rsrc 节最终会被链接到哪个虚拟地址，因此这里先写入
        节内偏移，并附上 IMAGE_REL_I386_DIR32NB 重定位项，
        由 Go 链接器在链接时自动加上节 VA。
    """
    HEADER_SIZE = 20
    SECHDR_SIZE = 40
    REL_ENTRY_SIZE = 10          # VirtualAddress(4) SymbolTableIndex(4) Type(2)
    SYM_ENTRY_SIZE = 18

    section_file_off = HEADER_SIZE + SECHDR_SIZE
    reloc_off = section_file_off + len(section_data)
    num_relocs = len(rva_fixups)
    symtab_off = reloc_off + num_relocs * REL_ENTRY_SIZE
    num_symbols = 2              # 主符号 + 1 个辅助符号
    strtab_off = symtab_off + num_symbols * SYM_ENTRY_SIZE

    # 字符串表：前 4 字节存总长度，随后是名称（含结束符）
    strtab_name = b".rsrc"
    strtab = struct.pack("<I", 4 + len(strtab_name) + 1) + strtab_name + b"\x00"

    # 主符号（18 字节）：名称(8) + Value(4) + SectionNumber(2) + Type(2)
    #                       + StorageClass(1) + NumberOfAuxSymbols(1)
    # 名称走字符串表：前 4 字节为 0，后 4 字节为字符串表内偏移
    sym_name = struct.pack("<II", 0, 4)
    sym_type = 0
    sym_class = 3               # IMAGE_SYM_CLASS_STATIC
    sym_aux = 1                 # 带 1 个辅助符号
    symbol = sym_name + struct.pack(
        "<IhHBB", 0, 1, sym_type, sym_class, sym_aux)
    # 辅助符号（18 字节）：Length(4) Relocs(2) Linenums(2) ... 补零到 18
    aux = struct.pack("<IHH", len(section_data), num_relocs, 0) + b"\x00" * 10
    symbol += aux

    # 重定位表：每条指向符号表第 0 项（.rsrc 节符号）
    relocs = b""
    for off in rva_fixups:
        relocs += struct.pack("<IIH", off, 0, IMAGE_REL_I386_DIR32NB)

    coff = io.BytesIO()
    # ---- COFF Header (20 字节)
    # Machine(2) NumberOfSections(2) TimeDateStamp(4) PointerToSymbolTable(4)
    # NumberOfSymbols(4) SizeOfOptionalHeader(2) Characteristics(2)
    coff.write(struct.pack(
        "<HHIIIHH",
        IMAGE_FILE_MACHINE_I386,          # Machine
        1,                                # NumberOfSections
        0,                                # TimeDateStamp
        symtab_off,                       # PointerToSymbolTable
        num_symbols,                      # NumberOfSymbols
        0,                                # SizeOfOptionalHeader（必须为 0）
        0,                                # Characteristics
    ))
    # ---- Section Header (40 字节)
    name = section_name + b"\x00" * (8 - len(section_name))
    characteristics = (IMAGE_SCN_CNT_INITIALIZED_DATA
                       | IMAGE_SCN_MEM_READ | IMAGE_SCN_MEM_WRITE)
    coff.write(struct.pack(
        "<8sIIIIIIHHI",
        name,
        len(section_data),      # VirtualSize
        0,                      # VirtualAddress
        len(section_data),      # SizeOfRawData
        section_file_off,       # PointerToRawData（节数据紧跟头部）
        reloc_off,              # PointerToRelocations
        0,                      # PointerToLinenumbers
        num_relocs,             # NumberOfRelocations
        0,                      # NumberOfLinenumbers
        characteristics,
    ))

    body = (coff.getvalue() + section_data + relocs + symbol + strtab)
    return body


# ---------------------------------------------------------------- 主流程

def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 1

    out_path = sys.argv[1]
    ico_path = sys.argv[2]
    version_str = sys.argv[3] if len(sys.argv) > 3 else "2.1.0"

    ver_parts = [int(x) for x in version_str.split(".")]
    while len(ver_parts) < 4:
        ver_parts.append(0)
    ver_tuple = tuple(ver_parts[:4])

    # ---- 读取 ICO
    with open(ico_path, "rb") as f:
        ico_data = f.read()
    icons = parse_ico(ico_data)
    print("图标: %d 个尺寸 -> %s" % (
        len(icons), ", ".join("%dx%d/%dbpp" % (e["width"], e["height"], e["bpp"])
                              for e in icons)))

    # ---- 构造资源
    ICON_ID_BASE = 1
    GROUP_ID = 1

    entries_by_type = {RT_ICON: [], RT_GROUP_ICON: [], RT_VERSION: []}

    for i, e in enumerate(icons):
        entries_by_type[RT_ICON].append(
            (ICON_ID_BASE + i, LANG_ZH_CN, e["data"]))

    group_data = build_group_icon(icons, ICON_ID_BASE)
    entries_by_type[RT_GROUP_ICON].append((GROUP_ID, LANG_ZH_CN, group_data))

    info = {
        "company": "playgitboy",
        "description": "PDF 批量压缩工具（离线版）",
        "file_version": ver_tuple,
        "file_version_str": version_str + ".0" if version_str.count(".") < 3
                             else version_str,
        "internal_name": "pdfcompress",
        "copyright": "Copyright (C) 2026 playgitboy",
        "original_filename": "pdfcompress.exe",
        "product_name": "PDFCompress",
        "product_version": ver_tuple,
        "product_version_str": version_str + ".0" if version_str.count(".") < 3
                               else version_str,
        "comments": "https://github.com/playgitboy/pdfcompress",
    }
    ver_data = build_version_info(info)
    entries_by_type[RT_VERSION].append((1, LANG_ZH_CN, ver_data))

    # ---- 生成 .rsrc 节 + COFF
    section, rva_fixups = build_rsrc_section(entries_by_type)
    blob = build_coff(section, rva_fixups)

    with open(out_path, "wb") as f:
        f.write(blob)

    print("已生成: %s (%d 字节)" % (out_path, len(blob)))
    print("  节大小 %d 字节，重定位 %d 条，图标 %d 个，版本信息 %d 字节"
          % (len(section), len(rva_fixups), len(icons), len(ver_data)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
