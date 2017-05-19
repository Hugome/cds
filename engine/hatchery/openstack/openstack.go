package openstack

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/tenantnetworks"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/images"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/spf13/viper"

	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/hatchery"
	"github.com/ovh/cds/sdk/log"
)

var hatcheryOpenStack *HatcheryCloud

var workersAlive map[string]int64

type ipInfos struct {
	workerName     string
	dateLastBooked time.Time
}

var ipsInfos = struct {
	mu  sync.RWMutex
	ips map[string]ipInfos
}{
	mu:  sync.RWMutex{},
	ips: map[string]ipInfos{},
}

// HatcheryCloud spawns instances of worker model with type 'ISO'
// by startup up virtual machines on /cloud
type HatcheryCloud struct {
	hatch    *sdk.Hatchery
	flavors  []flavors.Flavor
	networks []tenantnetworks.Network
	images   []images.Image
	client   *gophercloud.ServiceClient

	// User provided parameters
	address       string
	user          string
	password      string
	endpoint      string
	tenant        string
	region        string
	networkString string // flag from cli
	networkID     string // computed from networkString
	workerTTL     int
}

// ID returns hatchery id
func (h *HatcheryCloud) ID() int64 {
	if h.hatch == nil {
		return 0
	}
	return h.hatch.ID
}

//Hatchery returns hatchery instance
func (h *HatcheryCloud) Hatchery() *sdk.Hatchery {
	return h.hatch
}

// ModelType returns type of hatchery
func (*HatcheryCloud) ModelType() string {
	return sdk.Openstack
}

// CanSpawn return wether or not hatchery can spawn model
// requirements are not supported
func (h *HatcheryCloud) CanSpawn(model *sdk.Model, job *sdk.PipelineBuildJob) bool {
	for _, r := range job.Job.Action.Requirements {
		if r.Type == sdk.ServiceRequirement || r.Type == sdk.MemoryRequirement {
			return false
		}
	}
	return true
}

// Init fetch uri from nova
// then list available models
// then list available images
func (h *HatcheryCloud) Init() error {
	// Register without declaring model
	h.hatch = &sdk.Hatchery{
		Name: hatchery.GenerateName("openstack", viper.GetString("name")),
		UID:  viper.GetString("uk"),
	}

	workersAlive = map[string]int64{}

	authOpts := gophercloud.AuthOptions{
		Username:         h.user,
		Password:         h.password,
		AllowReauth:      true,
		IdentityEndpoint: h.address,
		TenantName:       h.tenant,
	}

	provider, errac := openstack.AuthenticatedClient(authOpts)
	if errac != nil {
		log.Error("Unable to openstack.AuthenticatedClient: %s", errac)
		os.Exit(11)
	}

	client, errn := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{Region: h.region})
	if errn != nil {
		log.Error("Unable to openstack.NewComputeV2: %s", errn)
		os.Exit(12)
	}
	h.client = client

	if err := h.initFlavors(); err != nil {
		log.Warning("Error getting flavors: %s", err)
	}

	if err := h.initNetworks(); err != nil {
		log.Warning("Error getting networks: %s", err)
	}

	h.initIPStatus()

	if errRegistrer := hatchery.Register(h.hatch, viper.GetString("token")); errRegistrer != nil {
		log.Warning("Cannot register hatchery: %s", errRegistrer)
	}

	go h.main()

	return nil
}

func (h *HatcheryCloud) initFlavors() error {
	all, err := flavors.ListDetail(h.client, nil).AllPages()
	if err != nil {
		return fmt.Errorf("initFlavors> error on flavors.ListDetail: %s", err)
	}
	lflavors, err := flavors.ExtractFlavors(all)
	if err != nil {
		return fmt.Errorf("initFlavors> error on flavors.ExtractFlavors: %s", err)
	}
	h.flavors = lflavors
	return nil
}

func (h *HatcheryCloud) initNetworks() error {
	all, err := tenantnetworks.List(h.client).AllPages()
	if err != nil {
		return fmt.Errorf("initNetworks> Unable to get Network: %s", err)
	}
	nets, err := tenantnetworks.ExtractNetworks(all)
	if err != nil {
		return fmt.Errorf("initNetworks> Unable to get Network: %s", err)
	}
	for _, n := range nets {
		if n.Name == h.networkString {
			h.networkID = n.ID
			break
		}
	}
	return nil
}

// initIPStatus initializes ipsInfos to
// add workername on ip belong to openstack-ip-range
// this func is called once, when hatchery is starting
func (h *HatcheryCloud) initIPStatus() {
	srvs := h.getServers()
	log.Info("initIPStatus> %d srvs", len(srvs))
	for ip := range ipsInfos.ips {
		log.Info("initIPStatus> checking %s", ip)
		for _, s := range srvs {
			if len(s.Addresses) == 0 {
				log.Info("initIPStatus> server %s - 0 addr", s.Name)
				continue
			}
			log.Debug("initIPStatus> server %s - work on %s", s.Name, h.networkString)
			all, errap := servers.ListAddressesByNetwork(h.client, s.ID, h.networkString).AllPages()
			if errap != nil {
				log.Error("initIPStatus> error on pager.AllPages %s", errap)
			}
			addrs, erren := servers.ExtractNetworkAddresses(all)
			if erren != nil {
				log.Error("initIPStatus> error on ExtractNetworkAddresses %s", erren)
			}
			for _, a := range addrs {
				log.Debug("initIPStatus> server %s - address %s (checking %s)", s.Name, a.Address, ip)
				if a.Address != "" && a.Address == ip {
					log.Info("initIPStatus> worker %s - use IP: %s", s.Name, a.Address)
					ipsInfos.ips[ip] = ipInfos{workerName: s.Name}
				}
			}

		}
	}
}

func (h *HatcheryCloud) main() {
	serverListTick := time.NewTicker(10 * time.Second).C
	killAwolServersTick := time.NewTicker(30 * time.Second).C
	killErrorServersTick := time.NewTicker(60 * time.Second).C
	killDisabledWorkersTick := time.NewTicker(60 * time.Second).C

	for {
		select {

		case <-serverListTick:
			h.updateServerList()

		case <-killAwolServersTick:
			h.killAwolServers()

		case <-killErrorServersTick:
			h.killErrorServers()

		case <-killDisabledWorkersTick:
			h.killDisabledWorkers()

		}
	}
}

func (h *HatcheryCloud) killDisabledWorkers() {
	workers, err := sdk.GetWorkers()
	if err != nil {
		log.Warning("killDisabledWorkers> Cannot fetch worker list: %s", err)
		return
	}

	srvs := h.getServers()

	for _, w := range workers {
		if w.Status != sdk.StatusDisabled {
			continue
		}

		for _, s := range srvs {
			if s.Name == w.Name {
				log.Info("Deleting disabled worker %s", w.Name)
				r := servers.Delete(h.client, s.ID)
				if err := r.ExtractErr(); err != nil {
					log.Warning("killDisabledWorkers> Cannot disabled worker %s: %s", s.Name, err)
					continue
				}
			}
		}
	}
}

func (h *HatcheryCloud) killErrorServers() {
	for _, s := range h.getServers() {
		//Remove server without IP Address
		if s.Status == "ACTIVE" {
			if len(s.Addresses) == 0 && time.Since(s.Updated) > 10*time.Minute {
				log.Info("Deleting server %s without IP Address", s.Name)

				r := servers.Delete(h.client, s.ID)
				if err := r.ExtractErr(); err != nil {
					log.Warning("killErrorServers> Cannot remove worker %s: %s", s.Name, err)
					continue
				}
			}
		}

		//Remove Error server
		if s.Status == "ERROR" {
			log.Info("Deleting server %s in error", s.Name)

			r := servers.Delete(h.client, s.ID)
			if err := r.ExtractErr(); err != nil {
				log.Warning("killErrorServers> Cannot remove worker in error %s: %s", s.Name, err)
				continue
			}
		}
	}
}

func (h *HatcheryCloud) killAwolServers() {
	workers, err := sdk.GetWorkers()
	now := time.Now().Unix()
	if err != nil {
		log.Warning("killAwolServers> Cannot fetch worker list: %s", err)
		return
	}

	for _, s := range h.getServers() {
		log.Debug("killAwolServers> Checking %s %v", s.Name, s.Metadata)
		if s.Status == "BUILD" {
			continue
		}

		var inWorkersList bool
		for _, w := range workers {
			if _, ok := workersAlive[w.Name]; !ok {
				log.Debug("killAwolServers> add %s to map workersAlive", w.Name)
				workersAlive[w.Name] = now
			}

			if w.Name == s.Name {
				inWorkersList = true
				workersAlive[w.Name] = now
				break
			}
		}

		workerHatcheryName, _ := s.Metadata["hatcheryName"]
		workerName, isWorker := s.Metadata["worker"]
		registerOnly, _ := s.Metadata["register_only"]
		workerModelName, ok := s.Metadata["worker_model_name"]
		if !ok {
			log.Debug("killAwolServers> error while getting worker model name from metadata:%s", err)
		}

		var toDeleteKilled bool
		if isWorker {
			if _, wasAlive := workersAlive[workerName]; wasAlive {
				if !inWorkersList {
					toDeleteKilled = true
					log.Debug("killAwolServers> %s toDeleteKilled --> true", workerName)
					delete(workersAlive, workerName)
				}
			}
		}

		// Delete workers, if not identified by CDS API
		// Wait for 6 minutes, to avoid killing worker babies
		log.Debug("killAwolServers> server %s status: %s last update: %s toDeleteKilled:%t inWorkersList:%t", s.Name, s.Status, time.Since(s.Updated), toDeleteKilled, inWorkersList)
		if isWorker && (workerHatcheryName == "" || workerHatcheryName == h.Hatchery().Name) &&
			(s.Status == "SHUTOFF" || toDeleteKilled || (!inWorkersList && time.Since(s.Updated) > 6*time.Minute)) {

			if s.Status == "SHUTOFF" && registerOnly == "true" && workerModelName != "" {
				log.Info("killAwolServers> create image before deleting server")
				imageID, err := servers.CreateImage(h.client, s.ID, servers.CreateImageOpts{
					Name: "cds-image-" + workerModelName,
					Metadata: map[string]string{
						"worker_model_name": workerModelName,
						"model":             s.Metadata["model"],
						"flavor":            s.Metadata["flavor"],
						"CreatedBy":         "cdsHatchery_" + h.Hatchery().Name,
					},
				}).ExtractImageID()
				if err != nil {
					log.Error("killAwolServers> error while create image for worker model %s", workerModelName)
				} else {
					log.Info("killAwolServers> image %s created for worker model %s - waiting 60s for saving created img...", imageID, workerModelName)
					time.Sleep(60 * time.Second)
					log.Debug("killAwolServers> end wait...", imageID, workerModelName)
				}
			}

			log.Info("killAwolServers> Deleting server %s status: %s last update: %s toDeleteKilled:%t inWorkersList:%t", s.Name, s.Status, time.Since(s.Updated), toDeleteKilled, inWorkersList)
			if err := servers.Delete(h.client, s.ID).ExtractErr(); err != nil {
				log.Warning("killAwolServers> Cannot remove server %s: %s", s.Name, err)
				continue
			}
		}
	}
	log.Debug("killAwolServers> workersAlive: %+v", workersAlive)
	// then clean workersAlive map
	toDelete := []string{}
	for workerName, t := range workersAlive {
		if t != now {
			toDelete = append(toDelete, workerName)
		}
	}
	for _, workerName := range toDelete {
		delete(workersAlive, workerName)
	}
}

// WorkersStarted returns the number of instances started but
// not necessarily register on CDS yet
func (h *HatcheryCloud) WorkersStarted() int {
	return len(h.getServers())
}

// WorkersStartedByModel returns the number of instances of given model started but
// not necessarily register on CDS yet
func (h *HatcheryCloud) WorkersStartedByModel(model *sdk.Model) int {
	var x int
	for _, s := range h.getServers() {
		if strings.Contains(s.Name, strings.ToLower(model.Name)) {
			x++
		}
	}
	log.Debug("WorkersStartedByModel> %s : %d", model.Name, x)
	return x
}

// KillWorker delete cloud instances
func (h *HatcheryCloud) KillWorker(worker sdk.Worker) error {
	log.Info("KillWorker> Kill %s", worker.Name)
	for _, s := range h.getServers() {
		if s.Name == worker.Name {
			r := servers.Delete(h.client, s.ID)
			if err := r.ExtractErr(); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("not found")
}

// SpawnWorker creates a new cloud instances
// requirements are not supported
func (h *HatcheryCloud) SpawnWorker(model *sdk.Model, job *sdk.PipelineBuildJob, registerOnly bool) error {
	if job != nil {
		log.Info("spawnWorker> spawning worker %s for job %d", model.Name, job.ID)
	} else {
		log.Info("spawnWorker> spawning worker %s ", model.Name)
	}

	var omd sdk.OpenstackModelData

	if h.hatch == nil {
		return fmt.Errorf("hatchery disconnected from engine")
	}

	if len(h.getServers()) == viper.GetInt("max-worker") {
		log.Debug("MaxWorker limit (%d) reached", viper.GetInt("max-worker"))
		return nil
	}

	if err := json.Unmarshal([]byte(model.Image), &omd); err != nil {
		return err
	}

	// Get image ID
	imageID, erri := h.imageID(omd.Image)
	if erri != nil {
		return erri
	}

	// Get flavor ID
	flavorID, errf := h.flavorID(omd.Flavor)
	if errf != nil {
		return errf
	}

	//generate a pretty cool name
	name := model.Name + "-" + strings.Replace(namesgenerator.GetRandomName(0), "_", "-", -1)

	// Ip len(ipsInfos.ips) > 0, specify one of those
	var ip string
	if len(ipsInfos.ips) > 0 {
		var errai error
		ip, errai = h.findAvailableIP(name)
		log.Debug("Found %s as first available IP", ip)
		if errai != nil {
			return errai
		}
	}

	// Decode base64 given user data
	udataModel, errd := base64.StdEncoding.DecodeString(omd.UserData)
	if errd != nil {
		return errd
	}

	graylog := ""
	if viper.GetString("graylog_host") != "" {
		graylog += fmt.Sprintf("export CDS_GRAYLOG_HOST=%s ", viper.GetString("graylog_host"))
	}
	if viper.GetString("graylog_port") != "" {
		graylog += fmt.Sprintf("export CDS_GRAYLOG_PORT=%s ", viper.GetString("graylog_port"))
	}
	if viper.GetString("graylog_extra_key") != "" {
		graylog += fmt.Sprintf("export CDS_GRAYLOG_EXTRA_KEY=%s ", viper.GetString("graylog_extra_key"))
	}
	if viper.GetString("graylog_extra_value") != "" {
		graylog += fmt.Sprintf("export CDS_GRAYLOG_EXTRA_VALUE=%s ", viper.GetString("graylog_extra_value"))
	}

	// Add curl of worker
	udataBegin := `#!/bin/sh
set +e
`
	udataEnd := `
cd $HOME
# Download and start worker with curl
curl  "{{.API}}/download/worker/$(uname -m)" -o worker --retry 10 --retry-max-time 120 -C - >> /tmp/user_data 2>&1
chmod +x worker
export CDS_SINGLE_USE=1
export CDS_FORCE_EXIT=1
export CDS_API={{.API}}
export CDS_KEY={{.Key}}
export CDS_NAME={{.Name}}
export CDS_MODEL={{.Model}}
export CDS_HATCHERY={{.Hatchery}}
export CDS_HATCHERY_NAME={{.HatcheryName}}
export CDS_BOOKED_JOB_ID={{.JobID}}
export CDS_TTL={{.TTL}}
{{.Graylog}}
./worker`

	if registerOnly {
		udataEnd += " register; "
	}
	udataEnd += " sudo shutdown -h now"

	var withExistingImage bool
	if !model.NeedRegistration && !registerOnly {
		start := time.Now()
		all, err := images.ListDetail(h.client, nil).AllPages()
		if err != nil {
			log.Error("spawnWorker> error on images.ListDetail: %s", err)
		}

		imgs, err := images.ExtractImages(all)
		if err != nil {
			log.Error("spawnWorker> error on images.ExtractImages: %s", err)
		}
		log.Debug("spawnWorker> call images.List on openstack took %fs, nbImages:%d", time.Since(start).Seconds(), len(imgs))
		for _, img := range imgs {
			workerModelName, _ := img.Metadata["worker_model_name"]
			if workerModelName == model.Name {
				withExistingImage = true
				log.Info("spawnWorker> existing image found for worker model %s img:%s", model.Name, img.ID)
				imageID = img.ID
				break
			}
		}
	}
	var udata string
	if withExistingImage {
		log.Debug("spawnWorker> using userdata from existing image")
		udata = udataBegin + udataEnd
	} else {
		log.Debug("spawnWorker> using userdata from worker model")
		udata = udataBegin + string(udataModel) + udataEnd
	}

	var jobID int64
	if job != nil {
		jobID = job.ID
	}

	tmpl, errt := template.New("udata").Parse(string(udata))
	if errt != nil {
		return errt
	}
	udataParam := struct {
		API          string
		Name         string
		Key          string
		Model        int64
		Hatchery     int64
		HatcheryName string
		JobID        int64
		TTL          int
		Graylog      string
	}{
		API:          viper.GetString("api"),
		Name:         name,
		Key:          viper.GetString("token"),
		Model:        model.ID,
		Hatchery:     h.hatch.ID,
		HatcheryName: h.hatch.Name,
		JobID:        jobID,
		TTL:          h.workerTTL,
		Graylog:      graylog,
	}
	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, udataParam); err != nil {
		return err
	}

	// Encode again
	udata64 := base64.StdEncoding.EncodeToString([]byte(buffer.String()))

	// Create openstack vm
	server, err := servers.Create(h.client, servers.CreateOpts{
		Name:      name,
		FlavorRef: flavorID,
		ImageRef:  imageID,
		Metadata: map[string]string{
			"worker":            name,
			"hatcheryName":      h.Hatchery().Name,
			"register_only":     fmt.Sprintf("%t", registerOnly),
			"flavor":            omd.Flavor,
			"model":             omd.Image,
			"worker_model_name": model.Name,
		},
		UserData: []byte(udata64),
		Networks: []servers.Network{{UUID: h.networkID, FixedIP: ip}},
	}).Extract()

	if err != nil {
		return fmt.Errorf("SpawnWorker> Unable to create server: %s", err)
	}
	log.Debug("SpawnWorker> Created Server ID: %s", server.ID)
	return nil
}

// Find image ID from image name
func (h *HatcheryCloud) imageID(img string) (string, error) {
	for _, i := range h.getImages() {
		if i.Name == img {
			return i.ID, nil
		}
	}
	return "", fmt.Errorf("imageID> image '%s' not found", img)
}

// Find flavor ID from flavor name
func (h *HatcheryCloud) flavorID(flavor string) (string, error) {
	for _, f := range h.flavors {
		if f.Name == flavor {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("flavorID> flavor '%s' not found", flavor)
}

// for each ip in the range, look for the first free ones
func (h *HatcheryCloud) findAvailableIP(workerName string) (string, error) {
	srvs := h.getServers()

	ipsInfos.mu.Lock()
	defer ipsInfos.mu.Unlock()
	for ip, infos := range ipsInfos.ips {
		// ip used less than 10s
		// 10s max to display worker name on nova list after call create
		if time.Since(infos.dateLastBooked) < 10*time.Second {
			continue
		}
		found := false
		for _, srv := range srvs {
			if infos.workerName == srv.Name {
				found = true
			}
		}
		if !found {
			infos.workerName = ""
			ipsInfos.ips[ip] = infos
		}
	}

	freeIP := []string{}
	for ip := range ipsInfos.ips {
		free := true
		if ipsInfos.ips[ip].workerName != "" {
			continue // ip already used by a worker
		}
		for _, s := range srvs {
			if len(s.Addresses) == 0 {
				continue
			}
			all, errap := servers.ListAddressesByNetwork(h.client, s.ID, h.networkString).AllPages()
			if errap != nil {
				log.Error("findAvailableIP> error on pager.AllPages %s", errap)
			}
			addrs, erren := servers.ExtractNetworkAddresses(all)
			if erren != nil {
				log.Error("findAvailableIP> error on ExtractNetworkAddresses %s", erren)
			}
			for _, a := range addrs {
				if a.Address == ip {
					free = false
				}
			}
			if !free {
				break
			}
		}
		if free {
			freeIP = append(freeIP, ip)
		}
	}

	if len(freeIP) == 0 {
		return "", fmt.Errorf("No IP left")
	}

	ipToBook := freeIP[rand.Intn(len(freeIP))]
	infos := ipInfos{
		workerName:     workerName,
		dateLastBooked: time.Now(),
	}
	ipsInfos.ips[ipToBook] = infos

	return ipToBook, nil
}

func (h *HatcheryCloud) updateServerList() {
	var out string
	var total int
	status := map[string]int{}

	for _, s := range h.getServers() {
		out += fmt.Sprintf("- [%s] %s:%s ", s.Updated, s.Status, s.Name)
		if _, ok := status[s.Status]; !ok {
			status[s.Status] = 0
		}
		status[s.Status]++
		total++
	}
	var st string
	for k, s := range status {
		st += fmt.Sprintf("%d %s ", s, k)
	}
	log.Info("Got %d servers %s", total, st)
	if total > 0 {
		log.Debug(out)
	}
}

//This a embeded cache for images list
var limages = struct {
	mu   sync.RWMutex
	list []images.Image
}{
	mu:   sync.RWMutex{},
	list: []images.Image{},
}

func (h *HatcheryCloud) getImages() []images.Image {
	t := time.Now()
	defer log.Debug("getImages(): %d s", time.Since(t).Seconds())

	limages.mu.RLock()
	nbImages := len(limages.list)
	limages.mu.RUnlock()

	if nbImages == 0 {
		all, err := images.ListDetail(h.client, nil).AllPages()
		if err != nil {
			log.Error("getImages> error on listDetail: %s", err)
			return limages.list
		}
		imgs, err := images.ExtractImages(all)
		if err != nil {
			log.Error("getImages> error on images.ExtractImages: %s", err)
			return limages.list
		}

		limages.mu.Lock()
		limages.list = imgs
		limages.mu.Unlock()
		//Remove data from the cache after 2 seconds
		go func() {
			time.Sleep(2 * time.Second)
			limages.mu.Lock()
			limages.list = []images.Image{}
			limages.mu.Unlock()
		}()
	}

	return limages.list
}

//This a embeded cache for servers list
var lservers = struct {
	mu   sync.RWMutex
	list []servers.Server
}{
	mu:   sync.RWMutex{},
	list: []servers.Server{},
}

func (h *HatcheryCloud) getServers() []servers.Server {
	t := time.Now()
	defer log.Debug("getServers() : %fs", time.Since(t).Seconds())

	lservers.mu.RLock()
	nbServers := len(lservers.list)
	lservers.mu.RUnlock()

	if nbServers == 0 {
		all, err := servers.List(h.client, nil).AllPages()
		if err != nil {
			log.Error("getServers> error on servers.List: %s", err)
			return lservers.list
		}
		serverList, err := servers.ExtractServers(all)
		if err != nil {
			log.Error("getServers> error on servers.ExtractServers: %s", err)
			return lservers.list
		}

		srvs := []servers.Server{}
		for _, s := range serverList {
			_, worker := s.Metadata["worker"]
			if !worker {
				continue
			}
			workerHatcheryName, _ := s.Metadata["hatcheryName"]
			if workerHatcheryName == "" || workerHatcheryName != h.Hatchery().Name {
				continue
			}
			srvs = append(srvs, s)
		}

		lservers.mu.Lock()
		lservers.list = srvs
		lservers.mu.Unlock()
		//Remove data from the cache after 2 seconds
		go func() {
			time.Sleep(2 * time.Second)
			lservers.mu.Lock()
			lservers.list = []servers.Server{}
			lservers.mu.Unlock()
		}()
	}

	return lservers.list
}

// IPinRanges returns a slice of all IP in all given IP ranges
// i.e 72.44.1.240/28,72.42.1.23/27
func IPinRanges(IPranges string) ([]string, error) {
	var ips []string

	ranges := strings.Split(IPranges, ",")
	for _, r := range ranges {
		i, err := IPinRange(r)
		if err != nil {
			return nil, err
		}
		ips = append(ips, i...)
	}
	return ips, nil
}

// IPinRange returns a slice of all IP in given IP range
// i.e 10.35.11.240/28
func IPinRange(IPrange string) ([]string, error) {
	ip, ipnet, err := net.ParseCIDR(IPrange)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip2 := ip.Mask(ipnet.Mask); ipnet.Contains(ip2); inc(ip2) {
		log.Info("Adding %s to IP pool", ip2)
		ips = append(ips, ip2.String())
	}

	return ips, nil
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
