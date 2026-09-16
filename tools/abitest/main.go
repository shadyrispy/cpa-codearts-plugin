// Command abitest loads the built plugin DLL exactly the way CLIProxyAPI does
// (syscall.LoadDLL + cliproxy_plugin_init with callback function pointers) and
// exercises the RPC surface. It is a development aid, not part of the plugin.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type cBuffer struct {
	ptr uintptr
	len uintptr
}

type hostAPI struct {
	abiVersion uint32
	hostCtx    uintptr
	call       uintptr
	freeBuffer uintptr
}

type pluginAPI struct {
	abiVersion uint32
	call       uintptr
	freeBuffer uintptr
	shutdown   uintptr
}

var logLines []string

func hostCall(_ uintptr, method uintptr, request uintptr, requestLen uintptr, response uintptr) uintptr {
	name := goString(method)
	body := goBytes(request, requestLen)
	logLines = append(logLines, fmt.Sprintf("HOSTCALL %s %s", name, string(body)))

	resp := (*cBuffer)(unsafe.Pointer(response))
	payload := []byte(`{"ok":true,"result":{}}`)
	switch name {
	case "host.log":
		payload = []byte(`{"ok":true,"result":{}}`)
	case "host.http.do":
		payload = []byte(`{"ok":true,"result":{"StatusCode":200,"Headers":{},"Body":"eyJvayI6dHJ1ZX0="}}`)
	default:
		payload = []byte(`{"ok":true,"result":{}}`)
	}
	buf, _ := syscall.BytePtrFromString(string(payload))
	_ = buf
	ptr := allocBytes(payload)
	resp.ptr = ptr
	resp.len = uintptr(len(payload))
	return 0
}

func hostFree(ptr uintptr, length uintptr) uintptr { return 0 }

var allocKeep [][]byte

func allocBytes(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	allocKeep = append(allocKeep, b)
	return uintptr(unsafe.Pointer(&allocKeep[len(allocKeep)-1][0]))
}

func goString(p uintptr) string {
	if p == 0 {
		return ""
	}
	var out []byte
	for i := uintptr(0); ; i++ {
		c := *(*byte)(unsafe.Pointer(p + i))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return string(out)
}

func goBytes(p uintptr, n uintptr) []byte {
	if p == 0 || n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), n)
}

func main() {
	path := os.Args[1]
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		fmt.Println("LoadDLL failed:", err)
		os.Exit(1)
	}
	proc, err := dll.FindProc("cliproxy_plugin_init")
	if err != nil {
		fmt.Println("FindProc failed:", err)
		os.Exit(1)
	}
	hostCtx := new(uintptr)
	api := &hostAPI{
		abiVersion: 1,
		hostCtx:    uintptr(unsafe.Pointer(hostCtx)),
		call:       syscall.NewCallback(hostCall),
		freeBuffer: syscall.NewCallback(hostFree),
	}
	var plugin pluginAPI
	rc, _, errCall := proc.Call(uintptr(unsafe.Pointer(api)), uintptr(unsafe.Pointer(&plugin)))
	fmt.Printf("cliproxy_plugin_init rc=%d err=%v abi=%d\n", rc, errCall, plugin.abiVersion)
	if rc != 0 || plugin.abiVersion != 1 || plugin.call == 0 {
		fmt.Println("INIT FAILED")
		os.Exit(1)
	}

	callPlugin := func(method, request string) {
		cMethod, _ := syscall.BytePtrFromString(method)
		var reqPtr uintptr
		if request != "" {
			rb := []byte(request)
			reqPtr = allocBytes(rb)
		}
		// Heap-allocate the response buffer exactly like the real host does
		// (windows.LocalAlloc + windowsBuffer). A Go stack local would be
		// invalidated by stack growth during the plugin's host callbacks.
		respMem, errAlloc := windows.LocalAlloc(windows.LMEM_FIXED|windows.LMEM_ZEROINIT, uint32(unsafe.Sizeof(cBuffer{})))
		if errAlloc != nil {
			fmt.Println("LocalAlloc failed:", errAlloc)
			os.Exit(1)
		}
		defer windows.LocalFree(windows.Handle(respMem))
		resp := (*cBuffer)(unsafe.Pointer(respMem))
		rcCall, _, _ := syscall.SyscallN(plugin.call,
			uintptr(unsafe.Pointer(cMethod)), reqPtr, uintptr(len(request)), respMem)
		runtime.KeepAlive(cMethod)
		runtime.KeepAlive(request)
		body := ""
		if resp.ptr != 0 && resp.len > 0 {
			body = string(goBytes(resp.ptr, resp.len))
			syscall.SyscallN(plugin.freeBuffer, resp.ptr, resp.len)
		}
		fmt.Printf("\n=== %s (rc=%d) len=%d ptr=%x ===\n", method, rcCall, len(body), resp.ptr)
		var pretty map[string]any
		if json.Unmarshal([]byte(body), &pretty) == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(out))
		} else {
			fmt.Println(body)
		}
	}

	registerPayload, errRead := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), "register.json"))
	if errRead != nil {
		fmt.Println("missing register.json:", errRead)
		os.Exit(1)
	}
	callPlugin("plugin.register", string(registerPayload))
	callPlugin("plugin.reconfigure", string(registerPayload))
	fmt.Println("--- repeat register to check for a first-call anomaly ---")
	callPlugin("plugin.register", string(registerPayload))
	callPlugin("model.register", `{}`)
	callPlugin("auth.identifier", `{}`)
	callPlugin("executor.identifier", `{}`)
	callPlugin("executor.count_tokens", `{"Payload":"aGVsbG8gd29ybGQgdGhpcyBpcyBhIHRlc3Q="}`)
	callPlugin("scheduler.pick", `{"Provider":"codearts-provider","Candidates":[{"ID":"codearts-provider-a.json","Provider":"codearts-provider"},{"ID":"codearts-provider-b.json","Provider":"codearts-provider"}]}`)
	callPlugin("scheduler.pick", `{"Provider":"codearts-provider","Candidates":[{"ID":"codearts-provider-a.json","Provider":"codearts-provider"},{"ID":"codearts-provider-b.json","Provider":"codearts-provider"}]}`)
	callPlugin("usage.handle", `{"Model":"PanguDev_COM_QC2","AuthID":"codearts-provider-a.json","Detail":{"InputTokens":10,"OutputTokens":5}}`)
	callPlugin("thinking.identifier", `{}`)
	callPlugin("thinking.apply", `{"Model":{"ID":"PanguDev_COM_QC2"},"Config":{"Mode":"high"},"Body":"eyJtb2RlbCI6Im0ifQ=="}`)
	callPlugin("quota.identifier", `{}`)
	callPlugin("quota.describe", `{}`)
	callPlugin("quota.fetch", `{"auth_index":"x"}`)
	callPlugin("quota.reset", `{}`)
	callPlugin("management.register", `{}`)
	callPlugin("management.handle", `{"Method":"GET","Path":"/v0/resource/plugins/codearts-provider/status"}`)
	callPlugin("management.handle", `{"Method":"GET","Path":"/v0/resource/plugins/codearts-provider/panel"}`)
	callPlugin("management.handle", `{"Method":"GET","Path":"/v0/management/codearts-provider/schedule"}`)
	callPlugin("management.handle", `{"Method":"POST","Path":"/v0/management/codearts-provider/quota/refresh","Body":"e30="}`)
	callPlugin("management.handle", `{"Method":"GET","Path":"/v0/management/codearts-provider/benefits"}`)
	callPlugin("management.handle", `{"Method":"GET","Path":"/v0/management/codearts-provider/usage"}`)
	callPlugin("management.handle", `{"Method":"POST","Path":"/v0/management/codearts-provider/checkin","Body":"e30="}`)
	callPlugin("bogus.method", `{}`)

	fmt.Println("\n=== host callbacks observed ===")
	for _, l := range logLines {
		if len(l) > 300 {
			l = l[:300] + "..."
		}
		fmt.Println(l)
	}
	_ = plugin.shutdown
	fmt.Println("\nABI TEST COMPLETE")
}
