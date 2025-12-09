package main

/*
#cgo LDFLAGS: -lsnap7
#include <stdlib.h>
#include <snap7.h>
*/
import "C"
import (
    "errors"
    "unsafe"
)

// srvArea* constants mirror Snap7 server area codes (not the client S7Area* codes).
const (
    srvAreaPE = 0 // Inputs
    srvAreaPA = 1 // Outputs
    srvAreaMK = 2 // Merkers
    srvAreaCT = 3 // Counters
    srvAreaTM = 4 // Timers
    srvAreaDB = 5 // Data blocks
)

// server param numbers (see snap7.h p_u16_LocalPort, ...).
const (
    paramLocalPort = 1
)

// S7Server is a light wrapper around Snap7 S7Object-based server.
type S7Server struct {
    obj C.S7Object
}

func NewS7Server() (*S7Server, error) {
    obj := C.Srv_Create()
    if obj == 0 {
        return nil, errors.New("Srv_Create returned nil")
    }
    return &S7Server{obj: obj}, nil
}

func (s *S7Server) RegisterArea(area int, index int, data []byte) int {
    if len(data) == 0 {
        return 0
    }
    return int(C.Srv_RegisterArea(s.obj, C.int(area), C.ushort(index), unsafe.Pointer(&data[0]), C.int(len(data))))
}

func (s *S7Server) StartTo(addr string) int {
    cstr := C.CString(addr)
    defer C.free(unsafe.Pointer(cstr))
    return int(C.Srv_StartTo(s.obj, cstr))
}

func (s *S7Server) Start() int {
    return int(C.Srv_Start(s.obj))
}

func (s *S7Server) Stop() int {
    return int(C.Srv_Stop(s.obj))
}

func (s *S7Server) Destroy() {
    C.Srv_Destroy((*C.S7Object)(&s.obj))
}

func (s *S7Server) ErrorText(code int) string {
    buf := make([]byte, 256)
    C.Srv_ErrorText(C.int(code), (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
    return C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
}

func (s *S7Server) SetParam(param int, value unsafe.Pointer) int {
    return int(C.Srv_SetParam(s.obj, C.int(param), value))
}

// SetPort updates the listening port before Start/StartTo.
func (s *S7Server) SetPort(port uint16) int {
    return s.SetParam(paramLocalPort, unsafe.Pointer(&port))
}
