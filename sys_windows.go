//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procGetDriveType     = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpace = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumeInfo    = kernel32.NewProc("GetVolumeInformationW")
	procSHFileOperation  = shell32.NewProc("SHFileOperationW")
)

type Drive struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
	Kind  string `json:"kind"`
}

func listDrives() []Drive {
	var out []Drive
	mask, _, _ := procGetLogicalDrives.Call()
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		kind := map[uintptr]string{2: "usb", 3: "ssd", 4: "net", 5: "cd"}[t]
		if kind == "" || kind == "cd" {
			continue
		}
		var freeAvail, total, totalFree uint64
		r, _, _ := procGetDiskFreeSpace.Call(uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&freeAvail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
		if r == 0 {
			continue
		}
		label := make([]uint16, 261)
		procGetVolumeInfo.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&label[0])), 261, 0, 0, 0, 0, 0)
		out = append(out, Drive{Path: root, Label: syscall.UTF16ToString(label), Total: int64(total), Free: int64(totalFree), Kind: kind})
	}
	return out
}

type shFileOpStruct struct {
	Hwnd                 uintptr
	Func                 uint32
	From                 *uint16
	To                   *uint16
	Flags                uint16
	AnyOperationsAborted int32
	NameMappings         uintptr
	ProgressTitle        *uint16
}

func moveToRecycleBin(path string) error {
	u, err := syscall.UTF16FromString(path) // already ends with one 0
	if err != nil {
		return err
	}
	u = append(u, 0) // list must be double-null terminated
	op := shFileOpStruct{
		Func:  3, // FO_DELETE
		From:  &u[0],
		Flags: 0x0040 | 0x0010 | 0x0004 | 0x0400, // ALLOWUNDO | NOCONFIRMATION | SILENT | NOERRORUI
	}
	r, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return errors.New("Windows error code " + itoa(int64(r)) + " (file in use or access denied?)")
	}
	if op.AnyOperationsAborted != 0 {
		return errors.New("operation aborted")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("item is still there (file may be in use)")
	}
	return nil
}

func revealInExplorer(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start() // explorer often returns exit code 1 even on success
}

func openWindow(url string) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
	}
	profile := filepath.Join(os.Getenv("LocalAppData"), "DiskMap", "webview")
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		cmd := exec.Command(c, "--app="+url, "--user-data-dir="+profile,
			"--window-size=1360,860", "--no-first-run", "--do-not-de-elevate", "--no-default-browser-check", "--disable-features=Translate")
		if err := cmd.Start(); err == nil {
			started := time.Now()
			cmd.Wait()
			if time.Since(started) > 4*time.Second {
				os.Exit(0) // window closed -> quit
			}
			return // process handed off to another instance; ping watchdog will stop us
		}
	}
	// fallback: default browser; the ping watchdog will stop the app later
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// fastScan reads the NTFS MFT of the volume containing path. Needs administrator rights.
func fastScan(path string, s *Scanner) (*Node, error) {
	abs, err := filepath.Abs(path)
	if err != nil || len(abs) < 3 || abs[1] != ':' {
		return nil, errors.New("not a local drive")
	}
	vol := strings.ToUpper(abs[:1]) + `:\`
	dev, _ := syscall.UTF16PtrFromString(`\\.\` + abs[:2])
	h, err := syscall.CreateFile(dev, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, errors.New("administrator rights are required to read the MFT")
	}
	f := os.NewFile(uintptr(h), `\\.\`+abs[:2])
	defer f.Close()
	s.current.Store("Reading MFT…")
	root, err := scanMFT(f, vol, s)
	if err != nil {
		return nil, err
	}
	rel := abs[3:]
	if rel == "" {
		return root, nil
	}
	n := subtree(root, rel)
	if n == nil {
		return nil, errors.New("folder not found in MFT")
	}
	n.Parent, n.Name = nil, path
	return n, nil
}
