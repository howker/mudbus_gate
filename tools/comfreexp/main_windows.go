// +build windows

package main

import (
    "fmt"
    "strconv"
    "strings"
    "syscall"
    "time"
    "unsafe"
)

const (
    wmDestroy = 0x0002
    wmCommand = 0x0111
    wmSetFont = 0x0030

    wsOverlappedWindow = 0x00CF0000
    wsVisible          = 0x10000000
    wsChild            = 0x40000000
    wsVScroll          = 0x00200000
    wsTabStop          = 0x00010000

    wsExClientEdge = 0x00000200

    cbsDropdown = 0x0002
    cbsAutoHScroll = 0x0040

    esMultiline   = 0x0004
    esAutoVScroll = 0x0040
    esReadOnly    = 0x0800

    bsPushButton = 0x00000000
    ssLeft       = 0x00000000

    swShow = 5

    colorBtnFace = 15
    idcArrow     = 32512

    cbAddString   = 0x0143
    cbResetContent = 0x014B
    cbSetCurSel   = 0x014E
    cbGetCurSel   = 0x0147
    cbGetLBText   = 0x0148
    cbGetLBTextLen = 0x0149

    emSetSel     = 0x00B1
    emReplaceSel = 0x00C2

    idCombo   = 1001
    idRefresh = 1002
    idCheck   = 1003
    idReset   = 1004

    genericRead  = 0x80000000
    genericWrite = 0x40000000
    openExisting = 3

    purgeTXAbort = 0x0001
    purgeRXAbort = 0x0002
    purgeTXClear = 0x0004
    purgeRXClear = 0x0008

    clrRTS = 4
    setRTS = 3
    clrDTR = 6
    setDTR = 5

    errorFileNotFound    = 2
    errorPathNotFound    = 3
    errorAccessDenied    = 5
    errorGenFailure      = 31
    errorSharingViolation = 32
    errorSemTimeout      = 121
    errorNoMoreItems     = 259

    digcfPresent = 0x00000002

    spdrpDeviceDesc   = 0x00000000
    spdrpFriendlyName = 0x0000000C

    difPropertyChange = 0x00000012
    dicsEnable        = 0x00000001
    dicsDisable       = 0x00000002
    dicsFlagGlobal    = 0x00000001
)

var (
    user32   = syscall.NewLazyDLL("user32.dll")
    kernel32 = syscall.NewLazyDLL("kernel32.dll")
    setupapi = syscall.NewLazyDLL("setupapi.dll")
    gdi32    = syscall.NewLazyDLL("gdi32.dll")

    procRegisterClassExW   = user32.NewProc("RegisterClassExW")
    procCreateWindowExW    = user32.NewProc("CreateWindowExW")
    procDefWindowProcW     = user32.NewProc("DefWindowProcW")
    procShowWindow         = user32.NewProc("ShowWindow")
    procUpdateWindow       = user32.NewProc("UpdateWindow")
    procGetMessageW        = user32.NewProc("GetMessageW")
    procTranslateMessage   = user32.NewProc("TranslateMessage")
    procDispatchMessageW   = user32.NewProc("DispatchMessageW")
    procPostQuitMessage    = user32.NewProc("PostQuitMessage")
    procLoadCursorW        = user32.NewProc("LoadCursorW")
    procSendMessageW       = user32.NewProc("SendMessageW")
    procGetWindowTextW     = user32.NewProc("GetWindowTextW")
    procSetWindowTextW     = user32.NewProc("SetWindowTextW")
    procEnableWindow       = user32.NewProc("EnableWindow")
    procMessageBoxW        = user32.NewProc("MessageBoxW")

    procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
    procCreateFileW        = kernel32.NewProc("CreateFileW")
    procCloseHandle        = kernel32.NewProc("CloseHandle")
    procQueryDosDeviceW    = kernel32.NewProc("QueryDosDeviceW")
    procPurgeComm          = kernel32.NewProc("PurgeComm")
    procEscapeCommFunction = kernel32.NewProc("EscapeCommFunction")

    procSetupDiGetClassDevsW              = setupapi.NewProc("SetupDiGetClassDevsW")
    procSetupDiEnumDeviceInfo             = setupapi.NewProc("SetupDiEnumDeviceInfo")
    procSetupDiGetDeviceRegistryPropertyW = setupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
    procSetupDiSetClassInstallParamsW     = setupapi.NewProc("SetupDiSetClassInstallParamsW")
    procSetupDiCallClassInstaller         = setupapi.NewProc("SetupDiCallClassInstaller")
    procSetupDiDestroyDeviceInfoList      = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

    procCreateFontW = gdi32.NewProc("CreateFontW")
)

type point struct {
    X int32
    Y int32
}

type msg struct {
    Hwnd    uintptr
    Message uint32
    WParam  uintptr
    LParam  uintptr
    Time    uint32
    Pt      point
}

type wndClassEx struct {
    CbSize        uint32
    Style         uint32
    LpfnWndProc   uintptr
    CbClsExtra    int32
    CbWndExtra    int32
    HInstance     uintptr
    HIcon         uintptr
    HCursor       uintptr
    HbrBackground uintptr
    LpszMenuName  *uint16
    LpszClassName *uint16
    HIconSm       uintptr
}

type guid struct {
    Data1 uint32
    Data2 uint16
    Data3 uint16
    Data4 [8]byte
}

type spDevinfoData struct {
    CbSize    uint32
    ClassGuid guid
    DevInst   uint32
    Reserved  uintptr
}

type spClassinstallHeader struct {
    CbSize          uint32
    InstallFunction uint32
}

type spPropchangeParams struct {
    ClassInstallHeader spClassinstallHeader
    StateChange        uint32
    Scope              uint32
    HwProfile          uint32
}

var portsClassGUID = guid{
    Data1: 0x4D36E978,
    Data2: 0xE325,
    Data3: 0x11CE,
    Data4: [8]byte{0xBF, 0xC1, 0x08, 0x00, 0x2B, 0xE1, 0x03, 0x18},
}

var (
    mainHwnd uintptr
    comboHwnd uintptr
    refreshHwnd uintptr
    checkHwnd uintptr
    resetHwnd uintptr
    logHwnd uintptr
    uiFont uintptr
)

func utf16Ptr(s string) *uint16 {
    p, err := syscall.UTF16PtrFromString(s)
    if err != nil {
        return nil
    }
    return p
}

func loword(v uintptr) uint16 { return uint16(v & 0xFFFF) }

func appendLog(s string) {
    if logHwnd == 0 {
        return
    }
    line := fmt.Sprintf("[%s] %s\r\n", time.Now().Format("15:04:05"), s)
    procSendMessageW.Call(logHwnd, emSetSel, ^uintptr(0), ^uintptr(0))
    procSendMessageW.Call(logHwnd, emReplaceSel, 0, uintptr(unsafe.Pointer(utf16Ptr(line))))
    procUpdateWindow.Call(logHwnd)
}

func setBusy(b bool) {
    enabled := uintptr(1)
    if b { enabled = 0 }
    procEnableWindow.Call(comboHwnd, enabled)
    procEnableWindow.Call(refreshHwnd, enabled)
    procEnableWindow.Call(checkHwnd, enabled)
    procEnableWindow.Call(resetHwnd, enabled)
    procUpdateWindow.Call(mainHwnd)
}

func selectedPort() (string, error) {
    buf := make([]uint16, 64)
    n, _, _ := procGetWindowTextW.Call(comboHwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
    if n == 0 {
        return "", fmt.Errorf("выберите COM-порт")
    }
    s := strings.TrimSpace(syscall.UTF16ToString(buf[:n]))
    s = strings.ToUpper(strings.Replace(s, " ", "", -1))
    if !strings.HasPrefix(s, "COM") {
        if _, err := strconv.Atoi(s); err == nil {
            s = "COM" + s
        }
    }
    if !strings.HasPrefix(s, "COM") {
        return "", fmt.Errorf("неверное имя порта: %s", s)
    }
    num, err := strconv.Atoi(strings.TrimPrefix(s, "COM"))
    if err != nil || num < 1 || num > 256 {
        return "", fmt.Errorf("неверное имя порта: %s", s)
    }
    return fmt.Sprintf("COM%d", num), nil
}

func queryPortExists(name string) bool {
    namep := utf16Ptr(name)
    if namep == nil { return false }
    buf := make([]uint16, 1024)
    r, _, _ := procQueryDosDeviceW.Call(
        uintptr(unsafe.Pointer(namep)),
        uintptr(unsafe.Pointer(&buf[0])),
        uintptr(len(buf)),
    )
    return r != 0
}

func refreshPorts() {
    current, _ := selectedPort()
    procSendMessageW.Call(comboHwnd, cbResetContent, 0, 0)
    first := -1
    currentIndex := -1
    count := 0
    for i := 1; i <= 256; i++ {
        name := fmt.Sprintf("COM%d", i)
        if !queryPortExists(name) { continue }
        idx, _, _ := procSendMessageW.Call(comboHwnd, cbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(name))))
        if first < 0 { first = int(idx) }
        if name == current { currentIndex = int(idx) }
        count++
    }
    if currentIndex >= 0 {
        procSendMessageW.Call(comboHwnd, cbSetCurSel, uintptr(currentIndex), 0)
    } else if first >= 0 {
        procSendMessageW.Call(comboHwnd, cbSetCurSel, uintptr(first), 0)
    }
    appendLog(fmt.Sprintf("Найдено COM-портов: %d", count))
}

func openPort(name string) (uintptr, error) {
    path := `\\.\` + name
    h, _, callErr := procCreateFileW.Call(
        uintptr(unsafe.Pointer(utf16Ptr(path))),
        genericRead|genericWrite,
        0,
        0,
        openExisting,
        0,
        0,
    )
    invalid := ^uintptr(0)
    if h == invalid {
        errno := uint32(0)
        if e, ok := callErr.(syscall.Errno); ok { errno = uint32(e) }
        switch errno {
        case errorAccessDenied, errorSharingViolation:
            return 0, fmt.Errorf("порт занят другим процессом или доступ запрещён (Windows error %d)", errno)
        case errorFileNotFound, errorPathNotFound:
            return 0, fmt.Errorf("порт не существует или сейчас отключён (Windows error %d)", errno)
        case errorGenFailure, errorSemTimeout:
            return 0, fmt.Errorf("драйвер порта не отвечает (Windows error %d)", errno)
        default:
            if errno == 0 { return 0, fmt.Errorf("не удалось открыть порт") }
            return 0, fmt.Errorf("не удалось открыть порт (Windows error %d)", errno)
        }
    }
    return h, nil
}

func closeHandle(h uintptr) {
    if h != 0 && h != ^uintptr(0) { procCloseHandle.Call(h) }
}

func checkPort() {
    port, err := selectedPort()
    if err != nil { appendLog("ERROR: " + err.Error()); return }
    appendLog("Проверяю " + port + "...")
    h, err := openPort(port)
    if err != nil {
        appendLog("ЗАНЯТ/НЕДОСТУПЕН: " + port + ": " + err.Error())
        return
    }
    closeHandle(h)
    appendLog("OK: " + port + " свободен и открывается эксклюзивно.")
}

func resetFreePort(port string, h uintptr) {
    procPurgeComm.Call(h, purgeTXAbort|purgeRXAbort|purgeTXClear|purgeRXClear)
    procEscapeCommFunction.Call(h, clrDTR)
    procEscapeCommFunction.Call(h, clrRTS)
    time.Sleep(200 * time.Millisecond)
    procEscapeCommFunction.Call(h, setDTR)
    procEscapeCommFunction.Call(h, setRTS)
    procPurgeComm.Call(h, purgeTXAbort|purgeRXAbort|purgeTXClear|purgeRXClear)
    closeHandle(h)
    appendLog("Порт был доступен: очищены RX/TX и сброшены DTR/RTS, дескриптор закрыт.")
}

func getDeviceText(h uintptr, info *spDevinfoData, prop uint32) string {
    buf := make([]byte, 1024)
    var regType uint32
    var required uint32
    r, _, _ := procSetupDiGetDeviceRegistryPropertyW.Call(
        h,
        uintptr(unsafe.Pointer(info)),
        uintptr(prop),
        uintptr(unsafe.Pointer(&regType)),
        uintptr(unsafe.Pointer(&buf[0])),
        uintptr(len(buf)),
        uintptr(unsafe.Pointer(&required)),
    )
    if r == 0 { return "" }
    words := (*[512]uint16)(unsafe.Pointer(&buf[0]))
    return syscall.UTF16ToString(words[:])
}

func findPortDevice(port string) (uintptr, spDevinfoData, string, error) {
    h, _, callErr := procSetupDiGetClassDevsW.Call(
        uintptr(unsafe.Pointer(&portsClassGUID)),
        0, 0, digcfPresent,
    )
    if h == ^uintptr(0) {
        return 0, spDevinfoData{}, "", fmt.Errorf("SetupDiGetClassDevs: %v", callErr)
    }
    needle := "(" + strings.ToUpper(port) + ")"
    for index := uint32(0); ; index++ {
        info := spDevinfoData{CbSize: uint32(unsafe.Sizeof(spDevinfoData{}))}
        r, _, err := procSetupDiEnumDeviceInfo.Call(h, uintptr(index), uintptr(unsafe.Pointer(&info)))
        if r == 0 {
            if e, ok := err.(syscall.Errno); ok && uint32(e) == errorNoMoreItems { break }
            break
        }
        text := getDeviceText(h, &info, spdrpFriendlyName)
        if text == "" { text = getDeviceText(h, &info, spdrpDeviceDesc) }
        if strings.Contains(strings.ToUpper(text), needle) {
            return h, info, text, nil
        }
    }
    procSetupDiDestroyDeviceInfoList.Call(h)
    return 0, spDevinfoData{}, "", fmt.Errorf("PnP-устройство для %s не найдено", port)
}

func propertyChange(h uintptr, info *spDevinfoData, state uint32) error {
    params := spPropchangeParams{
        ClassInstallHeader: spClassinstallHeader{
            CbSize: uint32(unsafe.Sizeof(spClassinstallHeader{})),
            InstallFunction: difPropertyChange,
        },
        StateChange: state,
        Scope: dicsFlagGlobal,
        HwProfile: 0,
    }
    r, _, err := procSetupDiSetClassInstallParamsW.Call(
        h,
        uintptr(unsafe.Pointer(info)),
        uintptr(unsafe.Pointer(&params.ClassInstallHeader)),
        uintptr(unsafe.Sizeof(params)),
    )
    if r == 0 { return fmt.Errorf("SetupDiSetClassInstallParams: %v", err) }
    r, _, err = procSetupDiCallClassInstaller.Call(difPropertyChange, h, uintptr(unsafe.Pointer(info)))
    if r == 0 { return fmt.Errorf("SetupDiCallClassInstaller: %v", err) }
    return nil
}

func resetPort() {
    port, err := selectedPort()
    if err != nil { appendLog("ERROR: " + err.Error()); return }
    setBusy(true)
    defer setBusy(false)

    appendLog("Освобождение/сброс " + port + "...")

    // First try the safe path. If the port opens, there is no foreign owner.
    if h, openErr := openPort(port); openErr == nil {
        appendLog("OK: " + port + " открылся. Выполняю мягкий сброс.")
        resetFreePort(port, h)
    } else {
        appendLog("Порт не открылся: " + openErr.Error())
        appendLog("Пробую перезапустить PnP-устройство порта. Чужой процесс НЕ завершаю.")
    }

    hDev, info, friendly, err := findPortDevice(port)
    if err != nil {
        appendLog("ERROR: " + err.Error())
        appendLog("Если порт удерживает программа, сначала закройте программу опроса и повторите.")
        return
    }
    defer procSetupDiDestroyDeviceInfoList.Call(hDev)
    appendLog("PnP: " + friendly)

    appendLog("Отключаю устройство Windows...")
    if err := propertyChange(hDev, &info, dicsDisable); err != nil {
        appendLog("ERROR: не удалось отключить устройство: " + err.Error())
        appendLog("На Windows XP проверьте права администратора.")
        return
    }
    time.Sleep(1200 * time.Millisecond)

    appendLog("Включаю устройство Windows...")
    if err := propertyChange(hDev, &info, dicsEnable); err != nil {
        appendLog("ERROR: не удалось включить устройство: " + err.Error())
        appendLog("Устройство отключено. Включите его через Диспетчер устройств или перезагрузите ПК.")
        return
    }
    time.Sleep(1800 * time.Millisecond)

    if h, openErr := openPort(port); openErr == nil {
        closeHandle(h)
        appendLog("OK: " + port + " после перезапуска свободен и снова открывается.")
    } else {
        appendLog("ВНИМАНИЕ: после перезапуска " + port + " всё ещё недоступен: " + openErr.Error())
        appendLog("Если программа опроса всё ещё запущена, она могла сразу снова открыть порт.")
    }
    refreshPorts()
}

func wndProc(hwnd uintptr, msgID uint32, wparam, lparam uintptr) uintptr {
    switch msgID {
    case wmCommand:
        switch loword(wparam) {
        case idRefresh:
            refreshPorts()
            return 0
        case idCheck:
            checkPort()
            return 0
        case idReset:
            resetPort()
            return 0
        }
    case wmDestroy:
        procPostQuitMessage.Call(0)
        return 0
    }
    r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msgID), wparam, lparam)
    return r
}

func createControl(exStyle uint32, class, text string, style uint32, x, y, w, h int32, parent uintptr, id int) uintptr {
    hwnd, _, _ := procCreateWindowExW.Call(
        uintptr(exStyle),
        uintptr(unsafe.Pointer(utf16Ptr(class))),
        uintptr(unsafe.Pointer(utf16Ptr(text))),
        uintptr(style),
        uintptr(x), uintptr(y), uintptr(w), uintptr(h),
        parent,
        uintptr(id),
        0, 0,
    )
    if uiFont != 0 && hwnd != 0 {
        procSendMessageW.Call(hwnd, wmSetFont, uiFont, 1)
    }
    return hwnd
}

func initUI(hwnd uintptr) {
    createControl(0, "STATIC", "COM-порт:", wsChild|wsVisible|ssLeft, 18, 18, 85, 22, hwnd, 0)
    comboHwnd = createControl(wsExClientEdge, "COMBOBOX", "", wsChild|wsVisible|wsTabStop|cbsDropdown|cbsAutoHScroll|wsVScroll, 105, 14, 125, 180, hwnd, idCombo)
    refreshHwnd = createControl(0, "BUTTON", "Обновить список", wsChild|wsVisible|wsTabStop|bsPushButton, 242, 13, 125, 27, hwnd, idRefresh)
    checkHwnd = createControl(0, "BUTTON", "Проверить", wsChild|wsVisible|wsTabStop|bsPushButton, 380, 13, 105, 27, hwnd, idCheck)
    resetHwnd = createControl(0, "BUTTON", "Освободить / сбросить", wsChild|wsVisible|wsTabStop|bsPushButton, 498, 13, 158, 27, hwnd, idReset)

    createControl(0, "STATIC", "Журнал:", wsChild|wsVisible|ssLeft, 18, 55, 100, 20, hwnd, 0)
    logHwnd = createControl(wsExClientEdge, "EDIT", "", wsChild|wsVisible|wsVScroll|esMultiline|esAutoVScroll|esReadOnly, 18, 77, 638, 310, hwnd, 0)
    createControl(0, "STATIC", "Утилита не завершает чужие процессы. При занятом/зависшем порте она перезапускает его PnP-устройство.", wsChild|wsVisible|ssLeft, 18, 397, 638, 38, hwnd, 0)

    refreshPorts()
    appendLog("COMFreeXP готов. Выберите порт и нажмите «Проверить» или «Освободить / сбросить».")
}

func main() {
    instance, _, _ := procGetModuleHandleW.Call(0)
    cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

    // Tahoma is present on Windows XP. Falling back to the stock font is harmless.
    fontName := utf16Ptr("Tahoma")
    uiFont, _, _ = procCreateFontW.Call(
        ^uintptr(12-1), 0, 0, 0, 400, 0, 0, 0,
        1, 0, 0, 0, 0,
        uintptr(unsafe.Pointer(fontName)),
    )

    className := utf16Ptr("COMFreeXPWindow")
    wc := wndClassEx{
        CbSize: uint32(unsafe.Sizeof(wndClassEx{})),
        LpfnWndProc: syscall.NewCallback(wndProc),
        HInstance: instance,
        HCursor: cursor,
        HbrBackground: colorBtnFace + 1,
        LpszClassName: className,
    }
    if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
        procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Не удалось зарегистрировать окно."))), uintptr(unsafe.Pointer(utf16Ptr("COMFreeXP"))), 0x10)
        return
    }

    hwnd, _, _ := procCreateWindowExW.Call(
        0,
        uintptr(unsafe.Pointer(className)),
        uintptr(unsafe.Pointer(utf16Ptr("COMFreeXP — освобождение COM-порта"))),
        wsOverlappedWindow|wsVisible,
        120, 100, 694, 486,
        0, 0, instance, 0,
    )
    if hwnd == 0 {
        procMessageBoxW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("Не удалось создать окно."))), uintptr(unsafe.Pointer(utf16Ptr("COMFreeXP"))), 0x10)
        return
    }
    mainHwnd = hwnd
    initUI(hwnd)
    procShowWindow.Call(hwnd, swShow)
    procUpdateWindow.Call(hwnd)

    var m msg
    for {
        r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
        if int32(r) <= 0 { break }
        procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
        procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
    }
}
