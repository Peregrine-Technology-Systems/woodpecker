// Copyright 2022 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"sort"
	"strings"
)

// FileMeta represents a file in version control.
type FileMeta struct {
	Name string
	Data []byte
}

// IsPipelineConfigFile reports whether a file name in a config folder could be a
// pipeline config (a .yaml/.yml file). It is the single source of truth shared by
// the config service's post-fetch filter and forge adapters that filter a config
// directory listing *before* fetching content — so a forge never fetches (and
// never exposes the whole config load to a transient error on) a file the config
// service would discard anyway, e.g. `*.disabled` (woodpecker#316). Keep these two
// call sites using this one predicate so they cannot drift and silently drop a
// valid config.
func IsPipelineConfigFile(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

type fileMetaList []*FileMeta

func (a fileMetaList) Len() int           { return len(a) }
func (a fileMetaList) Less(i, j int) bool { return a[i].Name < a[j].Name }
func (a fileMetaList) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func SortByName(fm []*FileMeta) []*FileMeta {
	l := fileMetaList(fm)
	sort.Sort(l)
	return l
}
