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

// srvArea* constants mirror Snap7 server area codes.
const (
    srvAreaPE = 0x81
    srvAreaPA = 0x82
    srvAreaMK = 0x83
    srvAreaDB = 0x84
    srvAreaCT = 0x1C
    srvAreaTM = 0x1D
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

func (s *S7Server) Stop() int {
    return int(C.Srv_Stop(s.obj))
}

func (s *S7Server) Destroy() {
    C.Srv_Destroy((*C.S7Object)(&s.obj))
}
