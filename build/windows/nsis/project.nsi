Unicode true

####
## Please note: Template replacements don't work in this file. They are provided with default defines like
## mentioned underneath.
## If the keyword is not defined, "wails_tools.nsh" will populate them.
## If they are defined here, "wails_tools.nsh" will not touch them. This allows you to use this project.nsi manually
## from outside of Wails for debugging and development of the installer.
## 
## For development first make a wails nsis build to populate the "wails_tools.nsh":
## > wails build --target windows/amd64 --nsis
## Then you can call makensis on this file with specifying the path to your binary:
## For a AMD64 only installer:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app.exe
## For a ARM64 only installer:
## > makensis -DARG_WAILS_ARM64_BINARY=..\..\bin\app.exe
## For a installer with both architectures:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app-amd64.exe -DARG_WAILS_ARM64_BINARY=..\..\bin\app-arm64.exe
####
## The following information is taken from the wails_tools.nsh file, but they can be overwritten here.
####
## !define INFO_PROJECTNAME    "my-project" # Default "foundation"
## !define INFO_COMPANYNAME    "My Company" # Default "My Company"
## !define INFO_PRODUCTNAME    "My Product Name" # Default "My Product"
## !define INFO_PRODUCTVERSION "1.0.0"     # Default "0.1.0"
## !define INFO_COPYRIGHT      "(c) Now, My Company" # Default "© 2026, My Company"
###
## !define PRODUCT_EXECUTABLE  "Application.exe"      # Default "${INFO_PROJECTNAME}.exe"
## !define UNINST_KEY_NAME     "UninstKeyInRegistry"  # Default "${INFO_COMPANYNAME}${INFO_PRODUCTNAME}"
####
## !define REQUEST_EXECUTION_LEVEL "admin"            # Default "admin"  see also https://nsis.sourceforge.io/Docs/Chapter4.html
## !define WAILS_INSTALL_SCOPE     "user"             # Default "machine" - set to "user" for per-user install ($LOCALAPPDATA) without UAC prompt
####
## Include the wails tools
####
!include "wails_tools.nsh"

# The version information for this two must consist of 4 parts
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

## 检测"是否已有实例在运行"要用到字符串查找（tasklist 输出里找进程名），
## 以及 uninstall 页的勾选框（nsDialogs）。两者都是 NSIS 自带头文件，无需插件。
!include "LogicLib.nsh"
!include "StrFunc.nsh"
!include "nsDialogs.nsh"
${StrLoc}

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
# !define MUI_WELCOMEFINISHPAGE_BITMAP "resources\leftimage.bmp" #Include this to add a bitmap on the left side of the Welcome Page. Must be a size of 164x314
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

!insertmacro MUI_PAGE_WELCOME # Welcome to the installer page.
# !insertmacro MUI_PAGE_LICENSE "resources\eula.txt" # Adds a EULA page to the installer
!insertmacro MUI_PAGE_DIRECTORY # In which folder install page.
Page custom ShortcutPage ShortcutPageLeave # 快捷方式选项（开始菜单 / 快捷栏 / 桌面）
!insertmacro MUI_PAGE_INSTFILES # Installing page.
!insertmacro MUI_PAGE_FINISH # Finished installation page.

!insertmacro MUI_UNPAGE_INSTFILES # Uninstalling page

## 卸载时是否连数据一起删 —— 默认**不删**。
##
## 数据（录音、转写结果、设置）在 $APPDATA\Mandaryn，与安装目录无关；删掉无法恢复，
## 所以默认保留，且必须由用户在独立页面显式勾选。这也与文档承诺一致：
## 卸载不删除用户数据（DEPLOYMENT §6.4）。
## 静默卸载（/S）不会进入这个页面，$RemoveUserData 保持未勾选状态 → 保留数据。
Var RemoveUserData
Var UnDataCheckbox

## 安装时的快捷方式选项：由独立页面提供，默认全选（保持既有行为）。
## 静默安装（/S）不进入自定义页面，因此默认值必须在 .onInit 里设好。
Var CreateStartMenu
Var CreateQuickLaunch
Var CreateDesktop
Var CbStartMenu
Var CbQuickLaunch
Var CbDesktop

UninstPage custom un.DataPage un.DataPageLeave

Function un.DataPage
    !insertmacro MUI_HEADER_TEXT "$(STR_UNINSTALL_OPTIONS)" "$(STR_KEEP_DATA_HINT)"
    nsDialogs::Create 1018
    Pop $0
    ${If} $0 == error
        Abort
    ${EndIf}
    ${NSD_CreateCheckbox} 0 0 100% 12u "$(STR_DELETE_DATA)"
    Pop $UnDataCheckbox
    ; 刻意不 Check：默认保留数据
    nsDialogs::Show
FunctionEnd

Function un.DataPageLeave
    ${NSD_GetState} $UnDataCheckbox $RemoveUserData
FunctionEnd

## 快捷方式选项页。
##
## 为什么单独一页而不是塞进「组件」页：组件页的勾选项与 Section 一一对应，
## 改动会牵动 wails_tools.nsh 的 Section 结构；自定义页只影响本文件，风险更小，
## 也与本安装器已有的「卸载选项」页风格一致（同样用 nsDialogs）。
Function ShortcutPage
    !insertmacro MUI_HEADER_TEXT "$(STR_SHORTCUT_TITLE)" "$(STR_SHORTCUT_SUBTITLE)"
    nsDialogs::Create 1018
    Pop $0
    ${If} $0 == error
        Abort
    ${EndIf}

    ${NSD_CreateCheckbox} 0 0 100% 12u "$(STR_SHORTCUT_STARTMENU)"
    Pop $CbStartMenu
    ${NSD_SetState} $CbStartMenu $CreateStartMenu

    ${NSD_CreateCheckbox} 0 16u 100% 12u "$(STR_SHORTCUT_QUICKLAUNCH)"
    Pop $CbQuickLaunch
    ${NSD_SetState} $CbQuickLaunch $CreateQuickLaunch

    ${NSD_CreateCheckbox} 0 32u 100% 12u "$(STR_SHORTCUT_DESKTOP)"
    Pop $CbDesktop
    ${NSD_SetState} $CbDesktop $CreateDesktop

    ${NSD_CreateLabel} 0 52u 100% 24u "$(STR_SHORTCUT_HINT)"
    Pop $0

    nsDialogs::Show
FunctionEnd

Function ShortcutPageLeave
    ${NSD_GetState} $CbStartMenu    $CreateStartMenu
    ${NSD_GetState} $CbQuickLaunch  $CreateQuickLaunch
    ${NSD_GetState} $CbDesktop      $CreateDesktop
FunctionEnd

## 安装器多语言：**跟随系统语言**，无需用户手动选择。
##
## MUI 的行为：按下面的声明顺序与系统 UI 语言做匹配，命中即用；都不命中则用第一个。
## 把简体中文放在首位会影响"非中文系统"的判定（MUI 优先匹配系统语言，位置只决定回退值），
## 因此这里保持 English 在首位作为回退，中文紧随其后。
## 这样：中文 Windows → 中文安装界面；其它语言 Windows → 英文界面。
## 静默安装（/S）不弹语言选择框，符合"跟随系统"的预期，不引入额外交互。
!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

## 安装器自定义文案的中英双语（其余内置页面文案由 MUI 语言包提供）
LangString STR_SHORTCUT_DESKTOP ${LANG_ENGLISH}  "Create a desktop shortcut"
LangString STR_SHORTCUT_DESKTOP ${LANG_SIMPCHINESE} "创建桌面快捷方式"
LangString STR_SHORTCUT_STARTMENU ${LANG_ENGLISH}  "Create a Start Menu shortcut"
LangString STR_SHORTCUT_STARTMENU ${LANG_SIMPCHINESE} "创建开始菜单快捷方式"
LangString STR_SHORTCUT_QUICKLAUNCH ${LANG_ENGLISH}  "Create a Quick Launch shortcut"
LangString STR_SHORTCUT_QUICKLAUNCH ${LANG_SIMPCHINESE} "创建快捷栏（快速启动）快捷方式"
LangString STR_SHORTCUT_TITLE ${LANG_ENGLISH}  "Shortcuts"
LangString STR_SHORTCUT_TITLE ${LANG_SIMPCHINESE} "快捷方式"
LangString STR_SHORTCUT_SUBTITLE ${LANG_ENGLISH}  "Choose where to create shortcuts."
LangString STR_SHORTCUT_SUBTITLE ${LANG_SIMPCHINESE} "选择要在哪些位置创建快捷方式。"
LangString STR_SHORTCUT_HINT ${LANG_ENGLISH}  "You can change this later by re-running the installer."
LangString STR_SHORTCUT_HINT ${LANG_SIMPCHINESE} "之后可重新运行安装程序修改这些选项。"
LangString STR_KEEP_DATA ${LANG_ENGLISH}  "Keep my recordings and settings (recommended)"
LangString STR_KEEP_DATA ${LANG_SIMPCHINESE} "保留我的录音与设置（推荐）"
LangString STR_RUNNING_WARN ${LANG_ENGLISH}  "Mandaryn is running. Please close it and try again."
LangString STR_RUNNING_WARN ${LANG_SIMPCHINESE} "Mandaryn 正在运行，请先关闭它再继续。"
LangString STR_UNINSTALL_OPTIONS ${LANG_ENGLISH}  "Uninstall options"
LangString STR_UNINSTALL_OPTIONS ${LANG_SIMPCHINESE} "卸载选项"
LangString STR_KEEP_DATA_HINT ${LANG_ENGLISH}  "Your recordings and settings are kept by default."
LangString STR_KEEP_DATA_HINT ${LANG_SIMPCHINESE} "默认保留你的录音与设置。"
LangString STR_DELETE_DATA ${LANG_ENGLISH}  "Also delete my recordings and settings (cannot be undone)"
LangString STR_DELETE_DATA ${LANG_SIMPCHINESE} "同时删除我的录音与设置（不可恢复）"

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\..\bin\${INFO_PROJECTNAME}-${INFO_PRODUCTVERSION}-windows-${ARCH}-installer.exe" # Name of the installer's file.
!if "${WAILS_INSTALL_SCOPE}" == "user"
    InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
!else
    InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
!endif
ShowInstDetails show # This will always show the installation details.

## 检测目标进程是否在运行。
##
## 为什么必须做：覆盖安装时 installer 要替换 $INSTDIR\mandaryn.exe，而运行中的 exe
## 被 Windows 锁定 → File 指令失败，用户看到的是「安装完成后程序还是旧版」或
## 一串 NSIS 错误弹窗。提前拦住并提示关闭，比事后排查便宜得多。
##
## 为什么用 tasklist 而不是 FindWindow：FindWindow 依赖窗口标题/类名（我们并不
## 控制标题，用户改语言或改标题就会漏判）；tasklist 按映像名匹配，与 UI 无关。
## nsExec 是 NSIS 自带插件，不需要额外分发 nsProcess.dll。
##
## 静默安装（/S）不能弹窗：无人值守环境弹窗会永久挂住 CI。但也**不能**放着不管 ——
## 实测过：应用运行时静默安装，NSIS 的 File 指令遇到锁定文件会失败，而退出码仍是 0，
## 结果装出一个空目录（"装完了但程序不在"），比报错更难查。
## 因此静默模式下直接以退出码 2 失败，让调用方（脚本/CI）看到明确的错误。
Function CheckAppNotRunning
    nsExec::ExecToStack 'cmd /c tasklist /FI "IMAGENAME eq ${PRODUCT_EXECUTABLE}" /NH'
    Pop $0          ; 退出码
    Pop $1          ; 输出

    ${StrLoc} $2 "$1" "${PRODUCT_EXECUTABLE}" ">"
    ${If} $2 != ""
        ${If} ${Silent}
            DetailPrint "${PRODUCT_EXECUTABLE} is running; aborting silent install (files are locked)."
            SetErrorLevel 2
            Quit
        ${Else}
            MessageBox MB_OK|MB_ICONSTOP "$(STR_RUNNING_WARN)"
            SetErrorLevel 2
            Quit
        ${EndIf}
    ${EndIf}
FunctionEnd

Function .onInit
   !insertmacro wails.checkArchitecture
   Call CheckAppNotRunning
   ; 默认全选：保持与旧版行为一致，也让 /S 静默安装仍然创建快捷方式。
   StrCpy $CreateStartMenu ${BST_CHECKED}
   StrCpy $CreateQuickLaunch ${BST_CHECKED}
   StrCpy $CreateDesktop ${BST_CHECKED}
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro wails.webview2runtime

    SetOutPath $INSTDIR
    
    !insertmacro wails.files

    ; 三个快捷方式都按用户在上一步的选择创建（默认全选，静默安装也走默认值）。
    ${If} $CreateStartMenu == ${BST_CHECKED}
        CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    ${EndIf}
    ${If} $CreateQuickLaunch == ${BST_CHECKED}
        CreateShortcut "$QUICKLAUNCH\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    ${EndIf}
    ${If} $CreateDesktop == ${BST_CHECKED}
        CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    ${EndIf}

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols
    
    !insertmacro wails.writeUninstaller
SectionEnd

Section "uninstall" 
    !insertmacro wails.setShellContext

    ; WebView2 的缓存目录（Wails v2 时代的路径，v3 未必存在；保留以免遗留垃圾）
    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    ; 用户数据：只有用户在卸载页显式勾选"同时删除我的录音与设置"才删。
    ; 注意目录名是 AppDirName（internal/paths/names.go = "Mandaryn"），
    ; 不是 PRODUCT_EXECUTABLE —— 后者带 .exe，是文件名不是目录名。
    ${If} $RemoveUserData == ${BST_CHECKED}
        RMDir /r "$APPDATA\${INFO_PRODUCTNAME}"
    ${EndIf}

    RMDir /r $INSTDIR

    ; 无论安装时是否创建，三个位置都尝试删除：Delete 对不存在的文件是空操作。
    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$QUICKLAUNCH\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    !insertmacro wails.deleteUninstaller
SectionEnd
