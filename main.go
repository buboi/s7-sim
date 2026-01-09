package main

import (
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ServerDef describes the PLC endpoint we emulate.
type ServerDef struct {
    ID      int
    Address string
    Rack    int
    Slot    int
    Port    int
    Cycle   time.Duration
}

// PointDef maps a measurement to a DB offset.
type PointDef struct {
	Key          string
	TankID       int
	Name         string
	Unit         string
	ServerID     int
	DB           int
	ByteOffset   int
	RegisterType string
	Multiplier   int
	DataType     string
	ControlDB    int
	ControlOff   int
	LowDB        int
	LowOff       int
	HighDB       int
	HighOff      int
}

// AlarmDef maps a single alarm bit.
type AlarmDef struct {
	ServerID   int
	DB         int
	ByteOffset int
	Bit        int
	Value      int
	Message    string
}

// StateDef describes a state bit we may toggle.
type StateDef struct {
	ServerID   int
	DB         int
	ByteOffset int
	Bit        int
	Value      int
	Area       string
}

// DosingDef maps a dosing chemical name to a DB/offset for amount.
type DosingDef struct {
	Name         string
	TankID       int
	DB           int
	ByteOffset   int
	RegisterType string
	Multiplier   int
	DataType     string
}

type Config struct {
	Server      ServerDef
	Points      []PointDef
	Alarms      []AlarmDef
	States      []StateDef
	Dosing      []DosingDef
	pointByKey  map[string]PointDef
	alarmByText map[string]AlarmDef
	doseByName  map[string]DosingDef
}

// Frame contains one snapshot of simulated data.
type Frame struct {
	Name   string             `json:"name"`
	WaitMS int                `json:"wait_ms"`
	States map[string]int     `json:"states"`
	Points map[string]float64 `json:"points"`
	CV     map[string]float64 `json:"cv"`
	LA     map[string]float64 `json:"la"`
	HA     map[string]float64 `json:"ha"`
	Words  map[string]float64 `json:"words"`
	Dosing map[string]float64 `json:"dosing"`
	Alarms []string           `json:"alarms"`
}

type SampleData struct {
	IntervalMS int     `json:"interval_ms"`
	Frames     []Frame `json:"frames"`
}

type Simulation struct {
	cfg       Config
	sample    SampleData
	dbAreas   map[int][]byte
	qArea     []byte
	dbMu      sync.Mutex
	srv       *S7Server
	listen    string
	port      int
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

func main() {
	configPath := flag.String("config", "topway-data0-s7.csv", "path to the CSV configuration")
	dataPath := flag.String("data", filepath.Join("simdata", "sample_values.json"), "path to the simulation values file")
    listen := flag.String("listen", "0.0.0.0", "IP address to bind the S7 server to (use empty to let Snap7 choose)")
    port := flag.Int("port", 0, "TCP port to listen on (default from CSV; fallback 1102 if none)")
    flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	log.Printf("loaded config %s (server=%s rack=%d slot=%d port=%d points=%d alarms=%d states=%d dosing=%d)",
		*configPath, cfg.Server.Address, cfg.Server.Rack, cfg.Server.Slot, cfg.Server.Port,
		len(cfg.Points), len(cfg.Alarms), len(cfg.States), len(cfg.Dosing))
	if len(cfg.Points) > 0 {
		p := cfg.Points[0]
		log.Printf("first point: tank=%d name=%q db=%d byte=%d reg=%s mult=%d type=%s cv=%d|%d la=%d|%d ha=%d|%d",
			p.TankID, p.Name, p.DB, p.ByteOffset, p.RegisterType, p.Multiplier, p.DataType,
			p.ControlDB, p.ControlOff, p.LowDB, p.LowOff, p.HighDB, p.HighOff)
	}
	if len(cfg.States) > 0 {
		st := cfg.States[0]
		log.Printf("first state: area=%s db=%d byte=%d bit=%d value=%d",
			st.Area, st.DB, st.ByteOffset, st.Bit, st.Value)
	}
	if len(cfg.Dosing) > 0 {
		d := cfg.Dosing[0]
		log.Printf("first dosing: tank=%d name=%q db=%d byte=%d reg=%s mult=%d type=%s",
			d.TankID, d.Name, d.DB, d.ByteOffset, d.RegisterType, d.Multiplier, d.DataType)
	}

	sample, err := loadSampleData(*dataPath)
	if err != nil {
		log.Fatalf("failed to load sample data: %v", err)
	}
	if len(sample.Frames) == 0 {
		log.Fatalf("sample data %s contains no frames", *dataPath)
	}
	firstFrame := sample.Frames[0]
	log.Printf("loaded sample %s (interval_ms=%d frames=%d first=%q states=%d points=%d cv=%d la=%d ha=%d dosing=%d alarms=%d)",
		*dataPath, sample.IntervalMS, len(sample.Frames), firstFrame.Name,
		len(firstFrame.States), len(firstFrame.Points), len(firstFrame.CV),
		len(firstFrame.LA), len(firstFrame.HA), len(firstFrame.Dosing), len(firstFrame.Alarms))
	for key, val := range firstFrame.Points {
		log.Printf("first sample point: %s=%.3f", key, val)
		break
	}
	for key, val := range firstFrame.States {
		log.Printf("first sample state: %s=%d", key, val)
		break
	}
	for key, val := range firstFrame.Dosing {
		log.Printf("first sample dosing: %s=%.3f", key, val)
		break
	}

    listenAddr := *listen
    if listenAddr == "" {
        listenAddr = cfg.Server.Address
    }
    portNum := *port
    if portNum == 0 {
        if cfg.Server.Port > 0 {
            portNum = cfg.Server.Port
        } else {
            portNum = 1102
        }
    }

    sim := newSimulation(cfg, sample, listenAddr, portNum)
	if err := sim.start(); err != nil {
		log.Fatalf("failed to start S7 server: %v", err)
	}
	defer sim.stop()

	log.Printf("server is ready on %s (DBs: %v)", listenAddr, sim.dbNumbers())

	interval := sample.IntervalMS
	if interval == 0 {
		interval = 2000
	}

	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	defer ticker.Stop()

	frameIdx := 0
	sim.applyFrame(sample.Frames[frameIdx])
	frameIdx = (frameIdx + 1) % len(sample.Frames)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			sim.applyFrame(sample.Frames[frameIdx])
			frameIdx = (frameIdx + 1) % len(sample.Frames)
		case <-sigCh:
			log.Printf("received signal, shutting down")
			return
		case <-sim.stoppedCh:
			return
		}
	}
}

func newSimulation(cfg Config, sample SampleData, listen string, port int) *Simulation {
	s := &Simulation{
		cfg:       cfg,
		sample:    sample,
		dbAreas:   map[int][]byte{},
		qArea:     []byte{},
		listen:    listen,
		port:      port,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}

	s.ensureDBLayout()
	s.ensureSampleLayout()
	s.applyDefaults()
	return s
}

func (s *Simulation) start() error {
	srv, err := NewS7Server()
	if err != nil {
		return err
	}
	if s.port > 0 {
		if res := srv.SetPort(uint16(s.port)); res != 0 {
			return fmt.Errorf("set port %d failed (code %d: %s)", s.port, res, srv.ErrorText(res))
		}
	}
	for dbNum, buf := range s.dbAreas {
		if res := srv.RegisterArea(srvAreaDB, dbNum, buf); res != 0 {
			return fmt.Errorf("register DB%d failed (code %d)", dbNum, res)
		}
	}
	if len(s.qArea) > 0 {
		if res := srv.RegisterArea(srvAreaPA, 0, s.qArea); res != 0 {
			return fmt.Errorf("register Q area failed (code %d)", res)
		}
	}

	var res int
	if s.listen == "" {
		res = srv.Start()
	} else {
		res = srv.StartTo(s.listen)
	}
	if res != 0 {
		return fmt.Errorf("start server failed (code %d: %s)", res, srv.ErrorText(res))
	}

	s.srv = srv
	return nil
}

func (s *Simulation) stop() {
	close(s.stopCh)
	if s.srv != nil {
		_ = s.srv.Stop()
		s.srv.Destroy()
	}
	close(s.stoppedCh)
}

func (s *Simulation) dbNumbers() []int {
	nums := make([]int, 0, len(s.dbAreas))
	for n := range s.dbAreas {
		nums = append(nums, n)
	}
	return nums
}

func (s *Simulation) ensureDBLayout() {
	for _, p := range s.cfg.Points {
		l := p.ByteOffset + registerLength(p.RegisterType, p.DataType)
		area := areaFromRegType(p.RegisterType)
		s.ensureSizeArea(area, p.DB, l)
		if p.ControlDB > 0 {
			s.ensureSize(p.ControlDB, p.ControlOff+registerLength(p.RegisterType, p.DataType))
		}
		if p.LowDB > 0 {
			s.ensureSize(p.LowDB, p.LowOff+registerLength(p.RegisterType, p.DataType))
		}
		if p.HighDB > 0 {
			s.ensureSize(p.HighDB, p.HighOff+registerLength(p.RegisterType, p.DataType))
		}
	}
	for _, a := range s.cfg.Alarms {
		s.ensureSize(a.DB, a.ByteOffset+1)
	}
	for _, st := range s.cfg.States {
		area := st.Area
		if area == "" {
			area = "db"
		}
		s.ensureSizeArea(area, st.DB, st.ByteOffset+1)
	}
	for _, d := range s.cfg.Dosing {
		l := d.ByteOffset + registerLength(d.RegisterType, d.DataType)
		area := areaFromRegType(d.RegisterType)
		s.ensureSizeArea(area, d.DB, l)
	}
}

// ensureSampleLayout grows DBs to fit any state bits referenced in sample frames.
// This prevents later resizes (and stale Snap7 registrations) when frames contain
// state bits outside the CSV-defined offsets.
func (s *Simulation) ensureSampleLayout() {
	for _, frame := range s.sample.Frames {
		for key := range frame.States {
			area, db, byt, _, err := parseStateKey(key)
			if err != nil {
				log.Printf("skip sizing state %q: %v", key, err)
				continue
			}
			s.ensureSizeArea(area, db, byt+1)
		}
		for key := range frame.Words {
			area, db, byt, err := parseWordKey(key)
			if err != nil {
				log.Printf("skip sizing word %q: %v", key, err)
				continue
			}
			s.ensureSizeArea(area, db, byt+2)
		}
	}
}

func (s *Simulation) applyDefaults() {
	// Set default states and alarm bits based on config values.
	for _, st := range s.cfg.States {
		area := st.Area
		if area == "" {
			area = "db"
		}
		s.setBit(area, st.DB, st.ByteOffset, st.Bit, st.Value != 0)
	}
	for _, a := range s.cfg.Alarms {
		s.setBit("db", a.DB, a.ByteOffset, a.Bit, a.Value != 0)
	}
}

func (s *Simulation) ensureSize(db int, required int) {
	buf, ok := s.dbAreas[db]
	if !ok || len(buf) < required {
		if !ok {
			buf = make([]byte, required)
		} else {
			bigger := make([]byte, required)
			copy(bigger, buf)
			buf = bigger
		}
		s.dbAreas[db] = buf
	}
}

func (s *Simulation) ensureSizeArea(area string, db int, required int) {
	switch strings.ToLower(area) {
	case "q":
		if len(s.qArea) < required {
			bigger := make([]byte, required)
			copy(bigger, s.qArea)
			s.qArea = bigger
		}
	default:
		s.ensureSize(db, required)
	}
}

func (s *Simulation) bufferForArea(area string, db int) []byte {
	if strings.ToLower(area) == "q" {
		return s.qArea
	}
	return s.dbAreas[db]
}

func (s *Simulation) applyFrame(frame Frame) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()

	// States: keys formatted as "<db>:<byte>:<bit>".
	for key, v := range frame.States {
		area, db, byt, bit, err := parseStateKey(key)
		if err != nil {
			log.Printf("skip state %q: %v", key, err)
			continue
		}
		s.ensureSizeArea(area, db, byt+1)
		s.setBit(area, db, byt, bit, v != 0)
	}

	// Points keyed by "<tankID>:<point name>".
	for key, val := range frame.Points {
		def, ok := s.cfg.pointByKey[key]
		if !ok {
			log.Printf("unknown point %q in frame %s", key, frame.Name)
			continue
		}
		if def.Multiplier == 0 {
			def.Multiplier = 1
		}
		raw := val * float64(def.Multiplier)
			area := areaFromRegType(def.RegisterType)
			s.writeValue(area, def.DB, def.ByteOffset, def.RegisterType, def.DataType, raw)
	}

	// Raw WORDs keyed by "DB:BYTE" (or "Q:BYTE").
	for key, val := range frame.Words {
		area, db, byt, err := parseWordKey(key)
		if err != nil {
			log.Printf("skip word %q: %v", key, err)
			continue
		}
		regType := "dbw"
		if area == "q" {
			regType = "q"
		}
		s.writeValue(area, db, byt, regType, "U16", val)
	}

	// Dosing amounts keyed by chemical name (matches CSV dosing name).
	for key, val := range frame.Dosing {
		def, ok := s.cfg.doseByName[key]
		if !ok {
			log.Printf("unknown dosing %q in frame %s", key, frame.Name)
			continue
		}
		mult := def.Multiplier
		if mult == 0 {
			mult = 1
		}
		raw := val * float64(mult)
		area := areaFromRegType(def.RegisterType)
		s.writeValue(area, def.DB, def.ByteOffset, def.RegisterType, def.DataType, raw)
	}

	// Control value (CV), low alarm (LA), high alarm (HA) writing to offsets in DB20.
	for key, val := range frame.CV {
		def, ok := s.cfg.pointByKey[key]
		if !ok || def.ControlDB == 0 {
			if !ok {
				log.Printf("unknown cv point %q in frame %s", key, frame.Name)
			}
			continue
		}
		raw := val * float64(def.Multiplier)
		area := areaFromRegType(def.RegisterType)
		s.writeValue(area, def.ControlDB, def.ControlOff, def.RegisterType, def.DataType, raw)
	}
	for key, val := range frame.LA {
		def, ok := s.cfg.pointByKey[key]
		if !ok || def.LowDB == 0 {
			if !ok {
				log.Printf("unknown la point %q in frame %s", key, frame.Name)
			}
			continue
		}
		raw := val * float64(def.Multiplier)
		area := areaFromRegType(def.RegisterType)
		s.writeValue(area, def.LowDB, def.LowOff, def.RegisterType, def.DataType, raw)
	}
	for key, val := range frame.HA {
		def, ok := s.cfg.pointByKey[key]
		if !ok || def.HighDB == 0 {
			if !ok {
				log.Printf("unknown ha point %q in frame %s", key, frame.Name)
			}
			continue
		}
		raw := val * float64(def.Multiplier)
		area := areaFromRegType(def.RegisterType)
		s.writeValue(area, def.HighDB, def.HighOff, def.RegisterType, def.DataType, raw)
	}

	// Alarms: clear all first, then set the ones listed for this frame.
	for _, a := range s.cfg.Alarms {
		s.setBit("db", a.DB, a.ByteOffset, a.Bit, false)
	}
	for _, name := range frame.Alarms {
		def, ok := s.cfg.alarmByText[name]
		if !ok {
			log.Printf("unknown alarm %q in frame %s", name, frame.Name)
			continue
		}
		s.setBit("db", def.DB, def.ByteOffset, def.Bit, true)
	}

	// log.Printf("applied frame %q", frame.Name)
}

func (s *Simulation) writeValue(area string, db, byteOffset int, regType, dataType string, value float64) {
	regType = strings.ToLower(regType)
	dataType = strings.ToUpper(dataType)

	switch regType {
	case "dbw", "dbx", "dbd", "q", "qx", "qdw":
		// WORDs, DWORDs, or single bits.
	default:
		log.Printf("unsupported register type %s", regType)
		return
	}

	switch dataType {
	case "U16", "UINT":
		if value < 0 {
			value = 0
		}
		s.ensureSizeArea(area, db, byteOffset+2)
		buf := s.bufferForArea(area, db)
		binary.BigEndian.PutUint16(buf[byteOffset:], uint16(value))
	case "S16", "INT":
		s.ensureSizeArea(area, db, byteOffset+2)
		buf := s.bufferForArea(area, db)
		binary.BigEndian.PutUint16(buf[byteOffset:], uint16(int16(value)))
	case "U32", "DINT":
		if value < 0 {
			value = 0
		}
		s.ensureSizeArea(area, db, byteOffset+4)
		buf := s.bufferForArea(area, db)
		binary.BigEndian.PutUint32(buf[byteOffset:], uint32(value))
	case "S32":
		s.ensureSizeArea(area, db, byteOffset+4)
		buf := s.bufferForArea(area, db)
		binary.BigEndian.PutUint32(buf[byteOffset:], uint32(int32(value)))
	case "REAL", "FLOAT", "F32":
		s.ensureSizeArea(area, db, byteOffset+4)
		buf := s.bufferForArea(area, db)
		bits := math.Float32bits(float32(value))
		binary.BigEndian.PutUint32(buf[byteOffset:], bits)
	default:
		// Default to 16-bit unsigned for unknown entries.
		s.ensureSizeArea(area, db, byteOffset+2)
		buf := s.bufferForArea(area, db)
		binary.BigEndian.PutUint16(buf[byteOffset:], uint16(value))
	}
}

func (s *Simulation) setBit(area string, db, byteOffset, bit int, on bool) {
	if bit < 0 || bit > 7 {
		log.Printf("invalid bit index %d", bit)
		return
	}
	s.ensureSizeArea(area, db, byteOffset+1)
	var buf []byte
	if strings.ToLower(area) == "q" {
		buf = s.qArea
	} else {
		buf = s.dbAreas[db]
	}
	mask := byte(1 << bit)
	if on {
		buf[byteOffset] |= mask
	} else {
		buf[byteOffset] &^= mask
	}
}

func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	var cfg Config
	cfg.pointByKey = map[string]PointDef{}
	cfg.alarmByText = map[string]AlarmDef{}
	cfg.doseByName = map[string]DosingDef{}

	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Config{}, err
		}
		if len(rec) == 0 {
			continue
		}

		tag := strings.TrimSpace(stripBOM(rec[0]))
		if tag == "" || strings.HasPrefix(tag, "#") {
			continue
		}

		switch tag {
        case "[server]":
            if len(rec) < 6 {
                continue
            }
            id, _ := strconv.Atoi(rec[1])
            addr, rack, slot, port := parseAddress(rec[3])
            cycle, _ := strconv.Atoi(rec[5])
            cfg.Server = ServerDef{ID: id, Address: addr, Rack: rack, Slot: slot, Cycle: time.Duration(cycle) * time.Millisecond, Port: port}
		case "[point]":
			if len(rec) < 10 {
				continue
			}
			tankID, _ := strconv.Atoi(rec[1])
			name := strings.TrimSpace(rec[2])
			unit := strings.TrimSpace(rec[3])
			serverID, _ := strconv.Atoi(rec[4])
			db, byteOffset, _ := parseDBOffset(rec[5])
			regType := strings.TrimSpace(rec[6])
			mult, _ := strconv.Atoi(defaultStr(rec[7], "1"))
			dataType := strings.TrimSpace(rec[8])
			ctrlDB, ctrlOff, _ := parseDBOffset(rec[9])
			lowDB, lowOff, _ := parseDBOffset(rec[10])
			highDB, highOff, _ := parseDBOffset(rec[11])
			key := fmt.Sprintf("%d:%s", tankID, name)
			def := PointDef{
				Key:          key,
				TankID:       tankID,
				Name:         name,
				Unit:         unit,
				ServerID:     serverID,
				DB:           db,
				ByteOffset:   byteOffset,
				RegisterType: regType,
				Multiplier:   mult,
				DataType:     dataType,
				ControlDB:    ctrlDB,
				ControlOff:   ctrlOff,
				LowDB:        lowDB,
				LowOff:       lowOff,
				HighDB:       highDB,
				HighOff:      highOff,
			}
			cfg.Points = append(cfg.Points, def)
			cfg.pointByKey[key] = def
		case "[alarm]":
			if len(rec) < 7 {
				continue
			}
			serverID, _ := strconv.Atoi(rec[1])
			db, byteOffset, _ := parseDBOffset(rec[2])
			bit, _ := strconv.Atoi(rec[3])
			val, _ := strconv.Atoi(rec[4])
			msg := strings.TrimSpace(rec[6])
			def := AlarmDef{ServerID: serverID, DB: db, ByteOffset: byteOffset, Bit: bit, Value: val, Message: msg}
			cfg.Alarms = append(cfg.Alarms, def)
			if msg != "" {
				cfg.alarmByText[msg] = def
			}
		case "[state-trigger]", "[state-dosing]":
			if len(rec) < 6 {
				continue
			}
			serverID, _ := strconv.Atoi(rec[1])
			db, byteOffset, _ := parseDBOffset(rec[2])
			regType := strings.TrimSpace(rec[3])
			area := areaFromRegType(regType)
			bit, _ := strconv.Atoi(rec[4])
			val, _ := strconv.Atoi(rec[5])
			def := StateDef{ServerID: serverID, DB: db, ByteOffset: byteOffset, Bit: bit, Value: val, Area: area}
			cfg.States = append(cfg.States, def)

			// For dosing rows, also capture amount target information.
			if tag == "[state-dosing]" && len(rec) >= 15 {
				tankID, _ := strconv.Atoi(rec[6])
				name := strings.TrimSpace(rec[9])
				doseDB, doseOff, _ := parseDBOffset(rec[10])
				doseRegType := strings.TrimSpace(rec[11])
				mult, _ := strconv.Atoi(defaultStr(rec[13], "1"))
				dataType := strings.TrimSpace(rec[14])
				doseDef := DosingDef{
					Name:         name,
					TankID:       tankID,
					DB:           doseDB,
					ByteOffset:   doseOff,
					RegisterType: doseRegType,
					Multiplier:   mult,
					DataType:     dataType,
				}
				cfg.Dosing = append(cfg.Dosing, doseDef)
				if name != "" {
					cfg.doseByName[name] = doseDef
				}
			}
		}
	}

	if cfg.Server.Address == "" {
		return Config{}, fmt.Errorf("no [server] entry found in %s", path)
	}
	return cfg, nil
}

func loadSampleData(path string) (SampleData, error) {
	f, err := os.Open(path)
	if err != nil {
		return SampleData{}, err
	}
	defer f.Close()

	var sample SampleData
	if err := json.NewDecoder(f).Decode(&sample); err != nil {
		return SampleData{}, err
	}
	return sample, nil
}

func parseDBOffset(raw string) (db int, byteOffset int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, fmt.Errorf("empty DB offset")
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ':' || r == '|' })
	if len(parts) == 1 {
		// Treat single number as DB number with zero offset.
		db, err = strconv.Atoi(parts[0])
		return db, 0, err
	}
	db, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	byteOffset, err = strconv.Atoi(parts[1])
	return
}

func parseAddress(raw string) (addr string, rack int, slot int, port int) {
    parts := strings.Split(raw, "|")
    if len(parts) >= 3 {
        addr = strings.TrimSpace(parts[0])
        rack, _ = strconv.Atoi(parts[1])
        slot, _ = strconv.Atoi(parts[2])
        if len(parts) >= 4 {
            port, _ = strconv.Atoi(parts[3])
        }
    }
    return
}

func parseStateKey(raw string) (area string, db int, byteOffset int, bit int, err error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		err = fmt.Errorf("state key should look like DB:BYTE:BIT")
		return
	}
	first := strings.ToLower(parts[0])
	if first == "q" {
		area = "q"
	} else {
		area = "db"
		if db, err = strconv.Atoi(parts[0]); err != nil {
			return
		}
	}
	if byteOffset, err = strconv.Atoi(parts[1]); err != nil {
		return
	}
	bit, err = strconv.Atoi(parts[2])
	return
}

func parseWordKey(raw string) (area string, db int, byteOffset int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, 0, fmt.Errorf("empty word key")
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "q:") {
		area = "q"
		raw = raw[2:]
	} else {
		area = "db"
	}
	if area == "q" {
		byteOffset, err = strconv.Atoi(raw)
		return
	}
	db, byteOffset, err = parseDBOffset(raw)
	return
}

func registerLength(regType, dataType string) int {
	regType = strings.ToLower(regType)
	dataType = strings.ToUpper(dataType)

	if regType == "dbx" || regType == "qx" || regType == "q" {
		return 1
	}
	if regType == "dbd" || regType == "qdw" {
		return 4
	}

	switch dataType {
	case "U32", "S32", "DINT", "REAL", "FLOAT":
		return 4
	default:
		return 2
	}
}

func areaFromRegType(regType string) string {
	switch strings.ToLower(regType) {
	case "q", "qx", "qb", "qw", "qdw":
		return "q"
	default:
		return "db"
	}
}

func defaultStr(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
