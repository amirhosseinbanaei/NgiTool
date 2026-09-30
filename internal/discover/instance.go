package discover

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
)

// Driver rebuilds the driver of a reported instance.
func Driver(env Env, in *Instance) driver.Driver {
	if in.Kind == KindHost {
		return driver.Host{Run: env.Run, Exe: in.Exe, Conf: in.Conf, Prefix: in.Prefix, Globals: in.Globals, Unit: in.Unit}
	}
	var ms []driver.Mount
	for _, m := range in.Mounts {
		if m.Type == "bind" || m.Type == "volume" {
			ms = append(ms, driver.Mount{Type: m.Type, Source: m.Source, Name: m.Name, Dest: m.Dest})
		}
	}
	conf := in.Conf
	if in.StockMain {
		conf = ""
	}
	return driver.Container{Run: env.Run, Name: in.Container, Image: in.Image, Bin: in.Bin, Conf: conf, Running: in.State == StateRunning, Mounts: ms}
}

func fileSource(in *Instance) nginxconf.Source {
	if in.Kind == KindHost {
		return nginxconf.FileSource{}
	}
	var ms []nginxconf.Mount
	for _, m := range in.Mounts {
		if m.Type == "bind" || m.Type == "volume" {
			ms = append(ms, nginxconf.Mount{Dest: m.Dest, Source: m.Source})
		}
	}
	src := nginxconf.Source(nginxconf.FileSource{Container: true, Mounts: ms})
	if in.StockMain {
		src = overlay{files: map[string]string{defaultConf: stockMain}, base: src}
	}
	return src
}

// Raw is the whole config as text, one "# configuration file" header per
// file: `nginx -T` for a running instance, else the files read one by one.
// It is printed and dropped, never stored (CONF-11).
func Raw(ctx context.Context, env Env, in *Instance) (string, error) {
	if in.State == StateRunning {
		out, err := Driver(env, in).Dump(ctx)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, driver.ErrStopped) && in.Source == "dump" {
			return "", err
		}
	}
	if len(in.Files) == 0 {
		return "", errors.New("the config of " + in.Name + " could not be read")
	}
	src := fileSource(in)
	var b strings.Builder
	for _, f := range in.Files {
		data, err := src.Read(f.Path)
		b.WriteString("# configuration file " + f.Path + ":\n")
		if err != nil {
			b.WriteString("# (" + errText(err) + ")\n\n")
			continue
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// Test runs `nginx -t` for a reported instance.
func Test(ctx context.Context, env Env, in *Instance) (driver.Result, error) {
	if !in.Caps.Test.OK {
		return driver.Result{}, errors.New(in.Caps.Test.Reason)
	}
	return Driver(env, in).Test(ctx)
}

// DockerAccess is the Docker status on its own, for doctor.
func DockerAccess(ctx context.Context, env Env) DockerStatus {
	return New(env).dockerStatus(ctx)
}

// SaveCache writes the report to path (0600). It holds the summaries only,
// never a dump (CONF-11).
func SaveCache(path string, r *Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return state.WriteFile(path, b, state.FileMode)
}

// LoadCache returns the cached report when it is younger than maxAge.
func LoadCache(path string, maxAge time.Duration, now time.Time) (*Report, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var r Report
	if json.Unmarshal(b, &r) != nil || r.Schema != Schema || now.Sub(r.ScannedAt) > maxAge || now.Before(r.ScannedAt) {
		return nil, false
	}
	return &r, true
}
