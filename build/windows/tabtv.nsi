; tabtv Windows 安装器（TT-T-29、TT-T-31，dev/04 R-7）。由 build/package.sh 调用：
;   makensis -DVERSION=<版本> -DSRC=<含 tabtv.exe 与 resources\ 的目录> -DOUT=<输出> -DICON=<图标> tabtv.nsi
;
; ⚠️ 装在当前用户目录（%LOCALAPPDATA%\Programs\tabtv），不要管理员权限：
;    应用内自动更新会以 /S 静默运行本安装器，要求管理员的话每次更新都弹 UAC，静默安装直接失败。
; ⚠️ 覆盖前等旧进程退出：自动更新时是「旧版本启动安装器 → 旧版本走正常退出（停库）」，
;    安装器比旧进程跑得快，不等就会撞上被占用的 tabtv.exe 与 PG 二进制。
; ⚠️ 静默安装（自动更新）完成后自己把新版本拉起来；手动安装由完成页的「运行 tabtv」勾选决定。
Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user

!include "MUI2.nsh"
!include "LogicLib.nsh"

Name "tabtv"
OutFile "${OUT}"
InstallDir "$LOCALAPPDATA\Programs\tabtv"
InstallDirRegKey HKCU "Software\tabtv" "InstallDir"
VIProductVersion "0.0.0.0"
VIAddVersionKey "ProductName" "tabtv"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "tabtv 安装程序"
VIAddVersionKey "LegalCopyright" "Apache-2.0"
VIAddVersionKey "FileVersion" "${VERSION}"

!define MUI_ICON "${ICON}"
!define MUI_UNICON "${ICON}"
!define MUI_FINISHPAGE_RUN "$INSTDIR\tabtv.exe"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"

; 最多等 60 秒让旧进程退出（正常退出包括停内嵌数据库，通常几秒）。
Function WaitForExit
  StrCpy $1 0
  ${Do}
    nsExec::ExecToStack 'cmd /c tasklist /FI "IMAGENAME eq tabtv.exe" /NH | find /I "tabtv.exe"'
    Pop $0
    ${If} $0 != 0
      ${Break}
    ${EndIf}
    IntOp $1 $1 + 1
    ${If} $1 > 60
      ${Break}
    ${EndIf}
    Sleep 1000
  ${Loop}
FunctionEnd

Section "tabtv" SecMain
  Call WaitForExit
  SetOutPath "$INSTDIR"
  ; 先删旧的随包资源：新版本可能删掉了某个文件，留着旧的会被误用。用户数据不在这里（在 %APPDATA%\tabtv）。
  RMDir /r "$INSTDIR\resources"
  File "${SRC}\tabtv.exe"
  File /r "${SRC}\resources"
  WriteUninstaller "$INSTDIR\uninstall.exe"
  WriteRegStr HKCU "Software\tabtv" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "DisplayName" "tabtv"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "Publisher" "tabtv contributors"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "DisplayIcon" "$INSTDIR\tabtv.exe"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "NoModify" 1
  WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv" "NoRepair" 1
  CreateDirectory "$SMPROGRAMS\tabtv"
  CreateShortcut "$SMPROGRAMS\tabtv\tabtv.lnk" "$INSTDIR\tabtv.exe"
  CreateShortcut "$DESKTOP\tabtv.lnk" "$INSTDIR\tabtv.exe"
SectionEnd

Function .onInstSuccess
  ${If} ${Silent}
    Exec '"$INSTDIR\tabtv.exe"'
  ${EndIf}
FunctionEnd

; ⚠️ 卸载不删用户数据（%APPDATA%\tabtv：数据库、素材、日志）：那是用户的作品，重装后还要用。
Section "Uninstall"
  Call un.WaitForExit
  RMDir /r "$INSTDIR\resources"
  Delete "$INSTDIR\tabtv.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\tabtv\tabtv.lnk"
  RMDir "$SMPROGRAMS\tabtv"
  Delete "$DESKTOP\tabtv.lnk"
  DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\tabtv"
  DeleteRegKey HKCU "Software\tabtv"
SectionEnd

Function un.WaitForExit
  StrCpy $1 0
  ${Do}
    nsExec::ExecToStack 'cmd /c tasklist /FI "IMAGENAME eq tabtv.exe" /NH | find /I "tabtv.exe"'
    Pop $0
    ${If} $0 != 0
      ${Break}
    ${EndIf}
    IntOp $1 $1 + 1
    ${If} $1 > 60
      ${Break}
    ${EndIf}
    Sleep 1000
  ${Loop}
FunctionEnd
