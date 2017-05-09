package api

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/mpolden/wakeonlan/wol"
)

type wake func(net.IP, net.HardwareAddr) error

type api struct {
	SourceIP  net.IP
	StaticDir string
	cacheFile string
	mu        sync.RWMutex
	wake
}

type Error struct {
	err     error
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type Devices struct {
	Devices []Device `json:"devices"`
}

type Device struct {
	MACAddress string `json:"macAddress"`
}

func (d *Devices) add(device Device) {
	for _, v := range d.Devices {
		if device.MACAddress == v.MACAddress {
			return
		}
	}
	d.Devices = append(d.Devices, device)
}

func (d *Devices) remove(device Device) {
	var keep []Device
	for _, v := range d.Devices {
		if device.MACAddress == v.MACAddress {
			continue
		}
		keep = append(keep, v)
	}
	d.Devices = keep
}

func New(cacheFile string) *api { return &api{cacheFile: cacheFile, wake: wol.Wake} }

func (a *api) readDevices() (*Devices, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	f, err := os.OpenFile(a.cacheFile, os.O_CREATE|os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var i Devices
	if len(data) == 0 {
		i.Devices = make([]Device, 0)
		return &i, nil
	}
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, err
	}
	if i.Devices == nil {
		i.Devices = make([]Device, 0)
	}
	return &i, nil
}

func (a *api) writeDevice(device Device, add bool) error {
	i, err := a.readDevices()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.cacheFile, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if add {
		i.add(device)
	} else {
		i.remove(device)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(i); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func (a *api) defaultHandler(w http.ResponseWriter, r *http.Request) (interface{}, *Error) {
	defer r.Body.Close()
	if r.Method == http.MethodGet {
		i, err := a.readDevices()
		if err != nil {
			return nil, &Error{err: err, Status: http.StatusInternalServerError, Message: "Could not unmarshal JSON"}
		}
		return i, nil
	}
	add := r.Method == http.MethodPost
	remove := r.Method == http.MethodDelete
	if add || remove {
		dec := json.NewDecoder(r.Body)
		var device Device
		if err := dec.Decode(&device); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Message: "Malformed JSON"}
		}
		if add {
			macAddress, err := net.ParseMAC(device.MACAddress)
			if err != nil {
				return nil, &Error{Status: http.StatusBadRequest, Message: fmt.Sprintf("Invalid MAC address: %s", device.MACAddress)}
			}
			if err := a.wake(a.SourceIP, macAddress); err != nil {
				return nil, &Error{Status: http.StatusBadRequest, Message: fmt.Sprintf("Failed to wake device with address %s", device.MACAddress)}
			}
		}
		if err := a.writeDevice(device, add); err != nil {
			return nil, &Error{err: err, Status: http.StatusInternalServerError, Message: "Could not unmarshal JSON"}
		}
		w.WriteHeader(http.StatusNoContent)
		return nil, nil
	}
	return nil, &Error{
		Status:  http.StatusMethodNotAllowed,
		Message: fmt.Sprintf("Invalid method %s, must be %s or %s", r.Method, http.MethodGet, http.MethodPost),
	}
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) (interface{}, *Error) {
	return nil, &Error{
		Status:  http.StatusNotFound,
		Message: "Resource not found",
	}
}

type appHandler func(http.ResponseWriter, *http.Request) (interface{}, *Error)

func (fn appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, e := fn(w, r)
	if e != nil { // e is *Error, not os.Error.
		if e.err != nil {
			log.Print(e.err)
		}
		out, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		w.WriteHeader(e.Status)
		w.Write(out)
	} else if data != nil {
		out, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		w.Write(out)
	}
}

func requestFilter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *api) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/wake", appHandler(a.defaultHandler))
	// Return 404 in JSON for all unknown requests under /api/
	mux.Handle("/api/", appHandler(notFoundHandler))
	if a.StaticDir != "" {
		fs := http.StripPrefix("/static/", http.FileServer(http.Dir(a.StaticDir)))
		mux.Handle("/static/", fs)
	}
	return requestFilter(mux)
}
