package guestman

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/onecloud/pkg/cloudcommon/httpclients"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/pkg/util/regutils"
)

type SGuestManager struct {
	ServersPath      string
	Servers          map[string]*SKVMGuestInstance
	CandidateServers map[string]*SKVMGuestInstance
	ServersLock      *sync.Mutex

	isLoaded bool
}

func NewSGuestManager(serversPath string) *SGuestManager {
	manager := &SGuestManager{}
	manager.ServersPath = serversPath
	manager.Servers = make(map[string]*SKVMGuestInstance, 0)
	manager.CandidateServers = make(map[string]*SKVMGuestInstance, 0)
	manager.ServersLock = &sync.Mutex{}
	manager.StartCpusetBalancer()
	manager.LoadExistingGuests()
	return manager
}

func (m *SGuestManager) Bootstrap() {
	if m.isLoaded || len(m.ServersPath) == 0 {
		log.Errorln("Guestman bootstrap has been called!!!!!")
	} else {
		m.isLoaded = true
		log.Infof("Loading existing guests ...")
		if len(m.CandidateServers) > 0 {
			m.VerifyExistingGuests(false)
		} else {
			m.OnLoadExistingGuestsComplete()
		}
	}
}

func (m *SGuestManager) VerifyExistingGuests(pendingDelete bool) {
	params := url.Values{
		"limit":          {"0"},
		"admin":          {"True"},
		"system":         {"True"},
		"pending_delete": {fmt.Sprintf("%s", pendingDelete)},
	}
	params.Set("filter.0", fmt.Sprintf("host_id.equals(%s)", "get host id //TODO"))
	if len(m.CandidateServers) > 0 {
		keys := make([]string, len(m.CandidateServers))
		var index = 0
		for k := range m.CandidateServers {
			keys[index] = k
			index++
		}
		params.Set("filter.1", strings.Join(keys, ","))
	}
	urlStr := fmt.Sprintf("/servers?%s", params.Encode())
	// TODO: get default context not use background context
	_, res, err := httpclients.GetDefaultComputeClient().Request(context.Background(), "GET", urlStr, nil, nil, false)
	if err != nil {
		m.OnVerifyExistingGuestsFail(err, pendingDelete)
	} else {
		m.OnVerifyExistingGuestsSucc(res, pendingDelete)
	}
}

func (m *SGuestManager) OnVerifyExistingGuestsFail(err error, pendingDelete bool) {
	log.Errorf("OnVerifyExistingGuestFail: %s, try again 30 seconds later", err.Error())
	AddTimeout(30*time.Second, func() { m.VerifyExistingGuests(false) })
}

func (m *SGuestManager) OnVerifyExistingGuestsSucc(res jsonutils.JSONObject, pendingDelete bool) {
	iServers, err := res.Get("servers")
	if err != nil {
		m.OnVerifyExistingGuestsFail(err, pendingDelete)
	} else {
		servers := iServers.(*jsonutils.JSONArray)
		for _, v := range servers.Value() {
			id, _ := v.GetString("id")
			server, ok := m.CandidateServers[id]
			if !ok {
				log.Errorf("verify_existing_guests return unknown server %s ???????", id)
			} else {
				server.ImportServer(pendingDelete)
			}
		}
		if !pendingDelete {
			m.VerifyExistingGuests(true)
		} else {
			var unknownServerrs = make([]*SKVMGuestInstance, 0)
			for _, server := range m.CandidateServers {
				log.Errorf("Server %s not found on this host", server.GetName())
				unknownServerrs = append(unknownServerrs, server)
			}
			for _, server := range unknownServerrs {
				m.RemoveCandidateServer(server)
			}
		}
	}
}

func (m *SGuestManager) RemoveCandidateServer(server *SKVMGuestInstance) {
	if _, ok := m.CandidateServers[server.GetId()]; ok {
		delete(m.CandidateServers, server.GetId())
		if len(m.CandidateServers) == 0 {
			m.OnLoadExistingGuestsComplete()
		}
	}
}

func (m *SGuestManager) OnLoadExistingGuestsComplete() {
	// TODO
}

func (m *SGuestManager) StartCpusetBalancer() {
	// TODO
}

func (m *SGuestManager) IsGuestDir(f os.FileInfo) bool {
	if !regutils.MatchUUID(f.Name()) {
		return false
	}
	if !f.Mode().IsDir() {
		return false
	}
	descFile := path.Join(m.ServersPath, f.Name(), "desc")
	if _, err := os.Stat(descFile); os.IsNotExist(err) {
		return false
	}
	return true
}

func (m *SGuestManager) LoadExistingGuests() {
	files, err := ioutil.ReadDir(m.ServersPath)
	if err != nil {
		log.Errorf("List servers path %s error %s", m.ServersPath, err)
	}
	for _, f := range files {
		if _, ok := m.Servers[f.Name()]; !ok && m.IsGuestDir(f) {
			log.Infof("Find existing guest %s", f.Name())
			m.LoadServer(f.Name())
		}
	}
}

func (m *SGuestManager) LoadServer(sid string) {
	guest := NewKVMGuestInstance(sid, m)
	err := guest.LoadDesc()
	if err != nil {
		log.Errorf("On load server error: %s", err)
		return
	}
	m.CandidateServers[sid] = guest
}

func (m *SGuestManager) PrepareCreate(sid string) error {
	m.ServersLock.Lock()
	defer m.ServersLock.Unlock()
	if _, ok := m.Servers[sid]; ok {
		return httperrors.NewBadRequestError("Guest %s exists", sid)
	}
	guest := NewKVMGuestInstance(sid, m)
	m.Servers[sid] = guest
	return guest.PrepareDir()
}

func (m *SGuestManager) Monitor(sid, cmd string, callback func(string)) error {
	if guest, ok := m.Servers[sid]; ok {
		if guest.IsRunning() {
			guest.monitor.SimpleCommand(cmd, callback)
			return nil
		} else {
			return httperrors.NewBadRequestError("Server stopped??")
		}
	} else {
		return httperrors.NewNotFoundError("Not found")
	}
}

func (m *SGuestManager) DoDeploy(ctx context.Context, sid string, body jsonutils.JSONObject, isInit bool) {

}

// delay cpuset balance
func (m *SGuestManager) CpusetBalance(ctx context.Context) {
	// TODO
}

func (m *SGuestManager) Status(sid string) string {
	if guest, ok := m.Servers[sid]; ok {
		// TODO
		// if guest.IsMaster() && !guest.IsMirrorJobSucc() {
		// 	return "block_stream"
		// }
		if guest.IsRunning() {
			return "running"
		} else if guest.IsSuspend() {
			return "suspend"
		} else {
			return "stopped"
		}
	} else {
		return "notfound"
	}
}

var guestManger *SGuestManager

func initGuestManager(serversPath string) {
	if guestManger == nil {
		guestManger = NewSGuestManager(serversPath)
	}
}

func GetGuestManager() *SGuestManager {
	return guestManger
}

func Stop() {
	// guestManger.ExitGuestCleanup()
}

func Init(serversPath string) {
	initGuestManager(serversPath)
}
