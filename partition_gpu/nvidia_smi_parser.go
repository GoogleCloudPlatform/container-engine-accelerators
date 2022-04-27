// Copyright 2022 Siemens AG. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang/glog"
)

type GPUPerInstanceProfiles = map[int]GPUAvailableProfiles

type GPUProfile struct{
	id int
	total int
}

type GPUAvailableProfiles struct {
	byname map[string]GPUProfile
}

func ParseMIGAvailableProfiles(lgip_output string) (GPUPerInstanceProfiles, error){
	profile_pattern_spec := `^\|\s+(\d+)\s+MIG\s+([^\s]+)\s+(\d+)\s+(\d+)\/(\d+).*\|$`
	profile_pattern := regexp.MustCompile(profile_pattern_spec)
	
	profiles := make(map[int]GPUAvailableProfiles)
	for _, line := range strings.Split(strings.TrimSuffix(lgip_output, "\n"), "\n") {
		matches := profile_pattern.FindStringSubmatch(line)
		if len(matches) == 0 {
			continue
		}
		glog.Infof("found profile: gpu: %s, profile: %-10s, id: %3s, free: %2s, total: %2s\n", matches[1], matches[2], matches[3], matches[4], matches[5])
		gpuid, _ := strconv.Atoi(matches[1])
		name := matches[2]
		profileid, _ := strconv.Atoi(matches[3])
		total, _ := strconv.Atoi(matches[5])

		if gpuid != 0 {
			return nil, errors.New("multi-gpu systems are not supported yet")
		}

		// assignment
		profile := profiles[gpuid]
		if profile.byname == nil {
			profile.byname = make(map[string]GPUProfile)
		}
		profile.byname[name] = GPUProfile{
			id: profileid,
			total: total,
		}
		profiles[gpuid]= profile
	}
	return profiles, nil
}
