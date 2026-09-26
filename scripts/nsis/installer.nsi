; VSay Agent Windows Installer
; NSIS Modern User Interface Script

!include "MUI2.nsh"
!include "FileFunc.nsh"

; --------------------------------
; General Configuration
; --------------------------------

!define PRODUCT_NAME "VSay Agent"
!define PRODUCT_PUBLISHER "VSay"
!define PRODUCT_WEB_SITE "https://vsay.in"
!define PRODUCT_DIR_REGKEY "Software\Microsoft\Windows\CurrentVersion\App Paths\vsay-agent.exe"
!define PRODUCT_UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
!define PRODUCT_UNINST_ROOT_KEY "HKLM"

; Version is passed during build
!ifndef VERSION
  !define VERSION "1.0.0"
!endif

!ifndef ARCH
  !define ARCH "amd64"
!endif

Name "${PRODUCT_NAME} ${VERSION}"
OutFile "vsay-agent-${VERSION}-windows-${ARCH}-setup.exe"
InstallDir "$PROGRAMFILES64\VSay\Agent"
InstallDirRegKey HKLM "${PRODUCT_DIR_REGKEY}" ""
RequestExecutionLevel admin
ShowInstDetails show
ShowUnInstDetails show

; --------------------------------
; MUI Settings
; --------------------------------

!define MUI_ABORTWARNING

; Welcome page
!insertmacro MUI_PAGE_WELCOME

; License page (optional)
; !insertmacro MUI_PAGE_LICENSE "..\..\LICENSE"

; Directory page
!insertmacro MUI_PAGE_DIRECTORY

; Installation page
!insertmacro MUI_PAGE_INSTFILES

; Finish page
!define MUI_FINISHPAGE_RUN "$INSTDIR\vsay-agent.exe"
!define MUI_FINISHPAGE_RUN_PARAMETERS "version"
!define MUI_FINISHPAGE_RUN_TEXT "Show version information"
!insertmacro MUI_PAGE_FINISH

; Uninstaller pages
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

; Language
!insertmacro MUI_LANGUAGE "English"

; --------------------------------
; Installer Sections
; --------------------------------

Section "Main Application" SecMain
  SectionIn RO

  ; Set output path to installation directory
  SetOutPath "$INSTDIR"
  SetOverwrite on

  ; Copy main executable
  File "..\..\dist\bin\windows-${ARCH}\vsay-agent.exe"

  ; Create config directory
  CreateDirectory "$APPDATA\VSay"

  ; Create start menu shortcuts
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\VSay Agent.lnk" "$INSTDIR\vsay-agent.exe"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

  ; Add to PATH
  EnVar::AddValue "PATH" "$INSTDIR"

  ; Write registry keys
  WriteRegStr HKLM "${PRODUCT_DIR_REGKEY}" "" "$INSTDIR\vsay-agent.exe"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "DisplayName" "$(^Name)"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "UninstallString" "$INSTDIR\uninstall.exe"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "DisplayIcon" "$INSTDIR\vsay-agent.exe"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "URLInfoAbout" "${PRODUCT_WEB_SITE}"
  WriteRegStr ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "Publisher" "${PRODUCT_PUBLISHER}"
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "NoModify" 1
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "NoRepair" 1

  ; Get and write install size
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}" "EstimatedSize" "$0"

  ; Create uninstaller
  WriteUninstaller "$INSTDIR\uninstall.exe"
SectionEnd

; --------------------------------
; Uninstaller Section
; --------------------------------

Section "Uninstall"
  ; Remove from PATH
  EnVar::DeleteValue "PATH" "$INSTDIR"

  ; Delete files
  Delete "$INSTDIR\vsay-agent.exe"
  Delete "$INSTDIR\uninstall.exe"

  ; Delete shortcuts
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\VSay Agent.lnk"
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk"
  RMDir "$SMPROGRAMS\${PRODUCT_NAME}"

  ; Delete installation directory
  RMDir "$INSTDIR"
  RMDir "$PROGRAMFILES64\VSay"

  ; Delete registry keys
  DeleteRegKey ${PRODUCT_UNINST_ROOT_KEY} "${PRODUCT_UNINST_KEY}"
  DeleteRegKey HKLM "${PRODUCT_DIR_REGKEY}"

  ; Optionally remove config (ask user)
  MessageBox MB_YESNO "Do you want to remove configuration files?" IDNO skip_config
    RMDir /r "$APPDATA\VSay"
  skip_config:

  SetAutoClose true
SectionEnd

; --------------------------------
; Section Descriptions
; --------------------------------

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain} "Install VSay Agent application and add to system PATH."
!insertmacro MUI_FUNCTION_DESCRIPTION_END
