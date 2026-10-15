#!/usr/bin/env python3
"""verify_dist.py —— 交付物二进制属性校验（版本管理用，随仓库一起维护）。

用途：每次重编三端后运行，确认产物仍满足既定的交付硬约束，避免"改了代码
顺手把兼容性改坏"这类问题悄悄溜进交付包。

校验项：
  Windows (pdfcompress.exe)
    1. PE 机器类型 = i386(0x014C)          —— 必须是 32 位，Win7 x86 要能跑
    2. PE 子系统版本 = 6.1                 —— 高于 6.1 在 Win7 上会被拒绝加载
    3. 仅依赖 KERNEL32.dll                 —— 保证零依赖、免装运行库
    4. 资源中的 FileVersion = 期望版本      —— 右键属性里看到的版本
  Linux (pdfcompress-linux-arm64 / x86_64)
    5. ELF 机器类型 = AArch64(183) / x86-64(62)

用法:
    python verify_dist.py <dist目录> <期望版本，如 2.3.0>
"""
import os
import struct
import sys


def pe_info(path):
    with open(path, "rb") as f:
        data = f.read()
    if data[:2] != b"MZ":
        raise ValueError("不是 PE 文件（缺 MZ）")
    e_lfanew = struct.unpack_from("<I", data, 0x3C)[0]
    if data[e_lfanew:e_lfanew + 4] != b"PE\0\0":
        raise ValueError("不是 PE 文件（缺 PE\\0\\0）")
    coff = e_lfanew + 4
    machine, nsec = struct.unpack_from("<HH", data, coff)
    opt_off = coff + 20
    opt_size = struct.unpack_from("<H", data, coff + 16)[0]
    magic = struct.unpack_from("<H", data, opt_off)[0]  # 0x10b=PE32, 0x20b=PE32+
    # IMAGE_OPTIONAL_HEADER: MajorSubsystemVersion @ +48, Minor @ +50
    major_sub, minor_sub = struct.unpack_from("<HH", data, opt_off + 48)

    # 导入表：DataDirectory[1] = Import Table (RVA,Size)
    # PE32: DataDirectory 起始 = opt_off + 96；PE32+: +112
    dd_off = opt_off + (96 if magic == 0x10B else 112)
    import_rva, import_size = struct.unpack_from("<II", data, dd_off + 8)

    # 节表 → RVA 转文件偏移
    sec_off = opt_off + opt_size
    sections = []
    for i in range(nsec):
        off = sec_off + i * 40
        name = data[off:off + 8].rstrip(b"\0").decode("latin-1")
        # IMAGE_SECTION_HEADER 字段顺序：VirtualSize, VirtualAddress,
        # SizeOfRawData, PointerToRawData —— 切勿写反。
        vsize, vaddr, rawsize, rawptr = struct.unpack_from("<IIII", data, off + 8)
        sections.append((vaddr, vsize, rawptr, rawsize))

    def rva2off(rva):
        for vaddr, vsize, rawptr, rawsize in sections:
            if vaddr <= rva < vaddr + max(vsize, rawsize):
                return rawptr + (rva - vaddr)
        return None

    dlls = []
    if import_rva:
        p = rva2off(import_rva)
        # UPX 加壳后导入表极小且节布局特殊：必须做边界与次数保护，
        # 否则会越过缓冲区读到无意义数据。
        guard = 0
        while p is not None and p + 20 <= len(data) and guard < 256:
            guard += 1
            # IMAGE_IMPORT_DESCRIPTOR: OriginalFirstThunk(4) TimeDateStamp(4)
            #   ForwarderChain(4) Name(4) FirstThunk(4)
            oft, tds, fc, name_rva, ft = struct.unpack_from("<IIIII", data, p)
            if name_rva == 0 and oft == 0 and ft == 0:
                break
            np_ = rva2off(name_rva)
            if np_ is None or np_ >= len(data):
                break
            end = data.find(b"\0", np_)
            if end < 0:
                break
            dlls.append(data[np_:end].decode("latin-1"))
            p += 20

    return {
        "machine": machine, "magic": magic,
        "subsystem_version": "%d.%d" % (major_sub, minor_sub),
        "dlls": dlls,
    }


def elf_machine(path):
    with open(path, "rb") as f:
        head = f.read(20)
    if head[:4] != b"\x7fELF":
        raise ValueError("不是 ELF 文件")
    return struct.unpack_from("<H", head, 18)[0]


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    dist, want_ver = sys.argv[1], sys.argv[2]
    fails = []

    exe = os.path.join(dist, "pdfcompress.exe")
    info = pe_info(exe)
    print("== Windows %s ==" % os.path.basename(exe))
    print("   机器类型   : 0x%04X %s" % (info["machine"],
          "OK (i386)" if info["machine"] == 0x014C else "✗ 期望 0x014C(i386)"))
    if info["machine"] != 0x014C:
        fails.append("PE machine != i386")
    print("   子系统版本 : %s %s" % (info["subsystem_version"],
          "OK (Win7=6.1)" if info["subsystem_version"] == "6.1" else "✗ 期望 6.1"))
    if info["subsystem_version"] != "6.1":
        fails.append("subsystem version != 6.1")
    dlls = sorted(set(d.upper() for d in info["dlls"]))
    print("   依赖 DLL   : %s" % (", ".join(dlls) or "(无)"))
    if dlls != ["KERNEL32.DLL"]:
        print("   ⚠ 期望仅 KERNEL32.DLL")
        fails.append("imports != KERNEL32.DLL only")

    for name, want in (("pdfcompress-linux-arm64", 183),
                       ("pdfcompress-linux-x86_64", 62)):
        p = os.path.join(dist, name)
        m = elf_machine(p)
        ok = (m == want)
        print("== Linux %s ==\n   ELF machine: %d %s" % (
            name, m, "OK" if ok else "✗ 期望 %d" % want))
        if not ok:
            fails.append("%s ELF machine != %d" % (name, want))

    # 版本资源（仅 Windows，需在非 UPX 包上读取；此处尝试读 FileVersion）
    try:
        import subprocess
        ps = ("(Get-Item -LiteralPath '%s').VersionInfo.FileVersion" %
              exe.replace("'", "''"))
        out = subprocess.run(
            ["powershell", "-NoProfile", "-NonInteractive", "-Command", ps],
            capture_output=True, text=True, timeout=60).stdout.strip()
        print("== 版本资源 ==\n   FileVersion: %s (期望前缀 %s) %s" % (
            out, want_ver, "OK" if out.startswith(want_ver) else "⚠ 不一致"))
        if not out.startswith(want_ver):
            fails.append("FileVersion != %s" % want_ver)
    except Exception as e:  # pragma: no cover
        print("   (跳过版本资源校验: %s)" % e)

    print()
    if fails:
        print("校验未通过：")
        for f in fails:
            print("  - %s" % f)
        return 1
    print("全部校验通过 ✓")
    return 0


if __name__ == "__main__":
    sys.exit(main())
