#!/bin/sh
# ============================================================
#  PDFCompress 拖放安装脚本（Linux / 麒麟 Kylin / 统信 UOS）
# ------------------------------------------------------------
#  作用：把本工具注册到桌面。之后即可像 Windows 一样，
#        把 PDF 文件或文件夹「拖到图标上」直接压缩。
#
#  用法：在本目录执行
#        sh 安装拖放.sh
# ============================================================

# 禁止 MSYS/Cygwin 做路径转换（在 Windows 的 Git Bash 下调试时需要；
# 在真实 Linux 上该变量无副作用）。
MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

# ---------- 定位自身目录（不依赖当前工作目录、支持符号链接） ----------
SELF="$0"
while [ -h "$SELF" ]; do
    DIR=$(cd -P "$(dirname "$SELF")" && pwd)
    LINK=$(readlink "$SELF")
    case "$LINK" in
        /*) SELF="$LINK" ;;
        *)  SELF="$DIR/$LINK" ;;
    esac
done
APP_DIR=$(cd -P "$(dirname "$SELF")" && pwd)

EXE="$APP_DIR/pdfcompress"
LAUNCHER="$APP_DIR/启动拖放.sh"

echo "============================================================"
echo " PDFCompress 拖放安装"
echo "============================================================"
echo " 程序目录: $APP_DIR"
echo

# ---------- 1. 基本检查 ----------
if [ ! -f "$EXE" ]; then
    echo "[错误] 未找到主程序: $EXE"
    echo "       请确认本脚本与 pdfcompress 在同一目录下。"
    exit 1
fi

if [ ! -d "$APP_DIR/gs" ]; then
    echo "[警告] 未找到内置压缩引擎目录 gs/，程序可能无法工作。"
    echo "       请确认使用完整的 tar.gz 发布包解压，而非零散拷贝的文件。"
    echo
fi

# ---------- 2. 修复可执行权限 ----------
echo "[1/4] 设置可执行权限..."
chmod 755 "$EXE" 2>/dev/null
[ -f "$APP_DIR/gs/bin/gs" ]      && chmod 755 "$APP_DIR/gs/bin/gs"      2>/dev/null
[ -f "$APP_DIR/gs/bin/gs.real" ] && chmod 755 "$APP_DIR/gs/bin/gs.real" 2>/dev/null
[ -d "$APP_DIR/gs/lib" ]         && chmod 755 "$APP_DIR"/gs/lib/*       2>/dev/null
echo "      完成"

# ---------- 3. 生成拖放启动器 ----------
echo "[2/4] 生成拖放启动器..."

# 启动器逻辑说明：
#   .desktop 的 Exec 会以 "<启动器> <文件1> <文件2> ..." 调用本脚本。
#   本脚本再找终端模拟器，在终端里运行主程序，并在结束后等待回车。
#
# 这里使用【单引号 heredoc】（<<'LAUNCHER_EOF'），内部所有 $ 与引号都保持
# 字面量，最后由 sed 注入真实路径与 RUNNER，避免多层嵌套展开出错。
cat > "$LAUNCHER" <<'LAUNCHER_EOF'
#!/bin/sh
# 由 PDFCompress.desktop 调用，参数为拖入的文件/文件夹路径。
APP_DIR="__APP_DIR_PLACEHOLDER__"
EXE="$APP_DIR/pdfcompress"

# 无参数（双击图标）：给出使用提示
if [ "$#" -eq 0 ]; then
    echo "PDFCompress - PDF 批量压缩工具"
    echo "------------------------------------------------------------"
    echo " 使用方式："
    echo "   1) 把 PDF 文件或文件夹【拖到本图标上】即可开始压缩"
    echo "   2) 或在终端中执行： $EXE -dir 目标目录 -r"
    echo
    printf " 按回车键关闭..."
    read _dummy
    exit 0
fi

# 在终端中运行主程序，结束后等待回车，避免窗口一闪而过。
# RUNNER 会作为参数交给内层 "sh -c" 执行，内层看不到外层的 EXE 变量，
# 因此这里必须写入【真实的程序绝对路径】。
RUNNER='__EXE_PLACEHOLDER__ "$@"; echo; printf "处理完成，按回车键关闭..."; read _x'

# 依次探测常见终端模拟器（麒麟/UOS 多为 ukui-terminal / deepin-terminal）
# 用 exec 替换当前进程：终端模拟器生命周期即本脚本生命周期，
# 不会出现"脚本已退出、窗口还在跑"的孤儿进程。
if command -v ukui-terminal >/dev/null 2>&1; then
    exec ukui-terminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v deepin-terminal >/dev/null 2>&1; then
    exec deepin-terminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v x-terminal-emulator >/dev/null 2>&1; then
    exec x-terminal-emulator -e "sh -c '$RUNNER' _ \"$@\""
elif command -v gnome-terminal >/dev/null 2>&1; then
    exec gnome-terminal --wait -- sh -c "$RUNNER" _ "$@"
elif command -v konsole >/dev/null 2>&1; then
    exec konsole -e sh -c "$RUNNER" _ "$@"
elif command -v xfce4-terminal >/dev/null 2>&1; then
    exec xfce4-terminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v mate-terminal >/dev/null 2>&1; then
    exec mate-terminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v qterminal >/dev/null 2>&1; then
    exec qterminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v lxterminal >/dev/null 2>&1; then
    exec lxterminal -e "sh -c '$RUNNER' _ \"$@\""
elif command -v xterm >/dev/null 2>&1; then
    exec xterm -e sh -c "$RUNNER" _ "$@"
fi

# 没找到终端模拟器：直接运行（输出不停留），并提示用户
"$EXE" "$@"
echo
echo "提示：未检测到终端模拟器，以上输出可能已滚出屏幕。"
echo "      如需查看完整结果，请在终端中手动执行："
echo "        $EXE -dir <目录> -r"
LAUNCHER_EOF

# 注入真实路径。
# RUNNER 中需要写入程序绝对路径（内层 sh -c 看不到外层变量）。
# 优先用 Python 做替换（对含空格/单引号的路径最安全），
# 若本机没有 python3，或脚本正运行在 Windows 的 Git Bash 调试环境下
# （Windows 原生 Python 不认识 /c/... 这类 MSYS 路径），则退化为 sed。
PY=""
if command -v python3 >/dev/null 2>&1; then
    # 仅在"非 Windows 原生 Python"时启用，避免 MSYS 路径不被识别
    if python3 -c 'import os,sys; sys.exit(0 if os.sep=="/" else 1)' 2>/dev/null; then
        PY="python3"
    fi
fi

if [ -n "$PY" ]; then
    PC_TARGET="$LAUNCHER" PC_APPDIR="$APP_DIR" PC_EXE="$EXE" "$PY" - <<'PYEOF'
import os
path = os.environ['PC_TARGET']
app_dir = os.environ['PC_APPDIR']
exe = os.environ['PC_EXE']
s = open(path, encoding='utf-8').read()
s = s.replace('__APP_DIR_PLACEHOLDER__', app_dir)
# RUNNER 用单引号包裹，路径中的单引号需用 '\'' 形式转义
safe = exe.replace("'", "'\\''")
s = s.replace('__EXE_PLACEHOLDER__', safe)
open(path, 'w', encoding='utf-8', newline='').write(s)
PYEOF
else
    # sed 兜底：转义 & 与 \ 以及分隔符 |
    esc_app=$(printf '%s' "$APP_DIR" | sed 's/[&\\|]/\\&/g')
    esc_exe=$(printf '%s' "$EXE"     | sed 's/[&\\|]/\\&/g')
    # RUNNER 位于单引号内，路径中的单引号用 '\'' 转义
    esc_exe=$(printf '%s' "$esc_exe" | sed "s/'/'\\\\''/g")
    TMP="$LAUNCHER.tmp.$$"
    sed "s|__APP_DIR_PLACEHOLDER__|$esc_app|g; s|__EXE_PLACEHOLDER__|$esc_exe|g" \
        "$LAUNCHER" > "$TMP" && mv "$TMP" "$LAUNCHER"
fi

chmod 755 "$LAUNCHER"

# 生成后做一次语法自检，避免因路径特殊字符导致启动器损坏
if ! sh -n "$LAUNCHER" 2>/dev/null; then
    echo "      [错误] 生成的启动器语法有误（可能因安装路径含单引号等特殊字符）。"
    echo "             请把本工具解压到不含单引号的目录后重新执行本脚本，"
    echo "             例如： /home/<用户名>/PDFCompress"
    exit 1
fi

echo "      完成: 启动拖放.sh"

# ---------- 4. 生成并安装 .desktop ----------
echo "[3/4] 生成桌面图标定义..."

DESKTOP_FILE="$APP_DIR/PDFCompress.desktop"

cat > "$DESKTOP_FILE" <<'DESKTOP_EOF'
[Desktop Entry]
Type=Application
Version=1.0
Name=PDF压缩
Name[zh_CN]=PDF压缩
GenericName=PDF 批量压缩
Comment=把 PDF 文件或文件夹拖到这里进行批量压缩
Comment[zh_CN]=把 PDF 文件或文件夹拖到这里进行批量压缩
Exec="__LAUNCHER_PLACEHOLDER__" %F
Icon=application-pdf
Terminal=false
Categories=Utility;Office;
MimeType=application/pdf;
NoDisplay=false
StartupNotify=true
DESKTOP_EOF

# 注入真实启动器路径。
# .desktop 的 Exec 中，路径已用双引号包裹，只需转义反斜杠与双引号。
esc_desk=$(printf '%s' "$LAUNCHER" | sed 's/[\\&|]/\\&/g; s/"/\\"/g')
TMP="$DESKTOP_FILE.tmp.$$"
sed "s|__LAUNCHER_PLACEHOLDER__|$esc_desk|g" "$DESKTOP_FILE" > "$TMP" && mv "$TMP" "$DESKTOP_FILE"

chmod 755 "$DESKTOP_FILE"
echo "      完成: PDFCompress.desktop"

# 安装到桌面 + 应用菜单
echo "[4/4] 创建桌面快捷方式..."

DESKTOP_DIR=""
for d in "$HOME/桌面" "$HOME/Desktop"; do
    if [ -d "$d" ]; then DESKTOP_DIR="$d"; break; fi
done

if [ -n "$DESKTOP_DIR" ]; then
    cp -f "$DESKTOP_FILE" "$DESKTOP_DIR/PDF压缩.desktop" 2>/dev/null
    chmod 755 "$DESKTOP_DIR/PDF压缩.desktop" 2>/dev/null
    # 麒麟/UOS 需要标记为"受信任"，否则图标不可用
    if command -v gio >/dev/null 2>&1; then
        gio set "$DESKTOP_DIR/PDF压缩.desktop" metadata::trusted true 2>/dev/null
    fi
    echo "      已创建: $DESKTOP_DIR/PDF压缩.desktop"
else
    echo "      未找到桌面目录，可手动把 PDFCompress.desktop 复制到桌面"
fi

APPS_DIR="$HOME/.local/share/applications"
if mkdir -p "$APPS_DIR" 2>/dev/null; then
    cp -f "$DESKTOP_FILE" "$APPS_DIR/pdfcompress.desktop" 2>/dev/null
    if command -v update-desktop-database >/dev/null 2>&1; then
        update-desktop-database "$APPS_DIR" >/dev/null 2>&1
    fi
fi

echo
echo "============================================================"
echo " 安装完成！"
echo "============================================================"
echo
echo " 使用方法："
echo "   1) 回到桌面，找到「PDF压缩」图标"
echo "   2) 选中一个或多个 PDF 文件（或文件夹），拖到该图标上"
echo "   3) 松手后弹出终端窗口，显示压缩进度，结束后按回车关闭"
echo
echo " 输出位置：与原文件同目录，文件名加 _ebook 后缀"
echo
echo " 若图标显示为\"未信任\"，右键选择\"允许启动\"即可。"
echo
