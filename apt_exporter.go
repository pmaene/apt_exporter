package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/fsnotify/fsnotify"
	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
)

type Package struct {
	Name         string
	Suites       []string
	Architecture string
}

const (
	CACHE_INSTALLED_PACKAGES   = "installed_packages"
	CACHE_UPGRADEABLE_PACKAGES = "upgradeable_packages"
)

var version = ""

var (
	aptUpDesc = prometheus.NewDesc(
		prometheus.BuildFQName("apt", "", "up"),
		"Whether collecting APT's metrics was successful.",
		nil,
		nil,
	)
	aptRebootRequiredDesc = prometheus.NewDesc(
		prometheus.BuildFQName("apt", "", "reboot_required"),
		"Whether a system restart is required.",
		nil,
		nil,
	)
)

func parseAptOutput(out []byte) []*Package {
	re := regexp.MustCompile(`^([^ ]+)\/([^ ]+) [^ ]+ ([^ ]+)`)

	ps := []*Package{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		ms := re.FindAllStringSubmatch(sc.Text(), -1)
		if len(ms) == 0 {
			continue
		}

		ps = append(
			ps,
			&Package{
				Name:         ms[0][1],
				Suites:       unique(strings.Split(ms[0][2], ",")),
				Architecture: ms[0][3],
			},
		)
	}

	return ps
}

func unique(src []string) []string {
	dst := []string{}

	mm := map[string]bool{}
	for _, v := range src {
		if !mm[v] {
			mm[v] = true
			dst = append(dst, v)
		}
	}

	return dst
}

type AptExporter struct {
	cache   *cache.Cache
	watcher *fsnotify.Watcher
}

func (e *AptExporter) cacheInstalledPackages() error {
	out, err := exec.Command("/usr/bin/apt", "list", "--installed").Output()
	if err != nil {
		return err
	}

	e.cache.Set(
		CACHE_INSTALLED_PACKAGES,
		parseAptOutput(out),
		cache.DefaultExpiration,
	)

	log.Infoln("Cached installed packages")
	return nil
}
func (e *AptExporter) cacheUpgradeablePackages() error {
	out, err := exec.Command("/usr/bin/apt", "list", "--upgradable").Output()
	if err != nil {
		return err
	}

	e.cache.Set(
		CACHE_UPGRADEABLE_PACKAGES,
		parseAptOutput(out),
		cache.DefaultExpiration,
	)

	log.Infoln("Cached upgradeable packages")
	return nil
}

func (e *AptExporter) collectInstalledPackages(ch chan<- prometheus.Metric) error {
	ps, f := e.cache.Get(CACHE_INSTALLED_PACKAGES)
	if !f {
		return fmt.Errorf(
			"Cache item with key \"%s\" does not exist",
			CACHE_INSTALLED_PACKAGES,
		)
	}

	aptPackagesInstalled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "apt_packages_installed",
			Help: "How many APT packages are installed by architecture and suite.",
		},
		[]string{"architecture", "suite"},
	)

	for _, p := range ps.([]*Package) {
		for _, s := range p.Suites {
			aptPackagesInstalled.WithLabelValues(s, p.Architecture).Inc()
		}
	}

	aptPackagesInstalled.Collect(ch)
	return nil
}
func (e *AptExporter) collectUpgradeablePackages(ch chan<- prometheus.Metric) error {
	ps, f := e.cache.Get(CACHE_UPGRADEABLE_PACKAGES)
	if !f {
		return fmt.Errorf(
			"Cache item with key \"%s\" does not exist",
			CACHE_UPGRADEABLE_PACKAGES,
		)
	}

	aptPackagesUpgradeable := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "apt_packages_upgradeable",
			Help: "How many APT packages are upgradeable by architecture and suite.",
		},
		[]string{"architecture", "suite"},
	)

	for _, p := range ps.([]*Package) {
		for _, s := range p.Suites {
			aptPackagesUpgradeable.WithLabelValues(p.Architecture, s).Inc()
		}
	}

	aptPackagesUpgradeable.Collect(ch)
	return nil
}
func (e *AptExporter) collectRebootRequired(ch chan<- prometheus.Metric) {
	_, err := os.Stat("/run/reboot-required")
	if os.IsNotExist(err) {
		ch <- prometheus.MustNewConstMetric(
			aptRebootRequiredDesc,
			prometheus.GaugeValue,
			0.0,
		)

		return
	}

	ch <- prometheus.MustNewConstMetric(
		aptRebootRequiredDesc,
		prometheus.GaugeValue,
		1.0,
	)
}

func (e *AptExporter) Close() {
	e.watcher.Close()
}
func (e *AptExporter) Watch() error {
	go func() {
		for {
			select {
			case evt, ok := <-e.watcher.Events:
				if !ok {
					return
				}

				switch evt.Name {
				case "/var/log/apt/history.log":
					if err := e.cacheInstalledPackages(); err != nil {
						log.Errorln(err)
					}
					if err := e.cacheUpgradeablePackages(); err != nil {
						log.Errorln(err)
					}

				case "/var/lib/apt/periodic/update-stamp":
					if err := e.cacheUpgradeablePackages(); err != nil {
						log.Errorln(err)
					}

				case "/var/lib/apt/periodic/update-success-stamp":
					if err := e.cacheUpgradeablePackages(); err != nil {
						log.Errorln(err)
					}
				}

			case err, ok := <-e.watcher.Errors:
				if !ok {
					return
				}

				log.Errorln(err)
			}
		}
	}()

	if err := e.cacheInstalledPackages(); err != nil {
		return err
	}
	if err := e.watcher.Add("/var/log/apt/history.log"); err != nil {
		return err
	}

	if err := e.cacheUpgradeablePackages(); err != nil {
		return err
	}
	if err := e.watcher.Add("/var/lib/apt/periodic/"); err != nil {
		return err
	}

	return nil
}

func (e *AptExporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- aptUpDesc
	ch <- aptRebootRequiredDesc
}
func (e *AptExporter) Collect(ch chan<- prometheus.Metric) {
	if err := e.collectInstalledPackages(ch); err != nil {
		ch <- prometheus.MustNewConstMetric(
			aptUpDesc,
			prometheus.GaugeValue,
			0.0,
		)

		return
	}

	if err := e.collectUpgradeablePackages(ch); err != nil {
		ch <- prometheus.MustNewConstMetric(
			aptUpDesc,
			prometheus.GaugeValue,
			0.0,
		)

		return
	}

	e.collectRebootRequired(ch)

	ch <- prometheus.MustNewConstMetric(
		aptUpDesc,
		prometheus.GaugeValue,
		1.0,
	)
}

func NewAptExporter() (*AptExporter, error) {
	c := cache.New(cache.NoExpiration, 0)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &AptExporter{
		cache:   c,
		watcher: w,
	}, nil
}

func getBuildInfo() debug.Module {
	bi, ok := debug.ReadBuildInfo()
	if ok {
		if version != "" {
			return debug.Module{
				Path:    bi.Main.Path,
				Version: version,
				Sum:     bi.Main.Sum,
				Replace: bi.Main.Replace,
			}
		}

		return bi.Main
	}

	return debug.Module{}
}

func init() {
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
}

func main() {
	var (
		listenAddress = kingpin.Flag(
			"web.listen-address",
			"Address on which to expose metrics and web interface.",
		).Default(":9509").String()
		metricsPath = kingpin.Flag(
			"web.telemetry-path",
			"Path under which to expose metrics.",
		).Default("/metrics").String()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(
		fmt.Sprintf(
			"%s %s compiled with %v on %v/%v",
			kingpin.CommandLine.Name,
			getBuildInfo().Version,
			runtime.Version(),
			runtime.GOOS,
			runtime.GOARCH,
		),
	)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting", kingpin.CommandLine.Name, getBuildInfo().Version)

	e, err := NewAptExporter()
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
	defer e.Close()

	if err := e.Watch(); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}

	prometheus.MustRegister(e)

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(
			[]byte(
				`<html>
				<head><title>APT Exporter</title></head>
				<body>
				<h1>APT Exporter</h1>
				<p><a href='` + *metricsPath + `'>Metrics</a></p>
				</body>
				</html>`,
			),
		)

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	log.Infoln("Listening on", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}
