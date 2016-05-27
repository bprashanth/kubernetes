/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	go_flag "flag"
	"github.com/spf13/pflag"
	"k8s.io/kubernetes/cmd/kube-dns/app"
	"k8s.io/kubernetes/cmd/kube-dns/app/options"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/version/verflag"
)

// No point piping this flag all around when what we really want is to just set
// the global verbosity level.
var logVerbosity = pflag.Bool("verbose-logs", false, "If set, logs are written at V(4), otherwise V(2).")

func main() {
	config := options.NewKubeDNSConfig()
	config.AddFlags(pflag.CommandLine)

	flag.InitFlags()
	if *logVerbosity {
		go_flag.Lookup("logtostderr").Value.Set("true")
		go_flag.Set("v", "4")
	}
	util.InitLogs()
	defer util.FlushLogs()

	verflag.PrintAndExitIfRequested()
	server := app.NewKubeDNSServerDefault(config)
	server.Run()
}
