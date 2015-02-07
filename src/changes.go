// Copyright 2013 Google Inc. All Rights Reserved.
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

package drive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	spinner "github.com/odeke-em/cli-spinner"
)

type dirList struct {
	remote *File
	local  *File
}

func (d *dirList) Name() string {
	if d.remote != nil {
		return d.remote.Name
	}
	return d.local.Name
}

type sizeCounter struct {
	count int64
	src   int64
	dest  int64
}

func (t *sizeCounter) String() string {
	str := fmt.Sprintf("count %v", t.count)
	if t.src > 0 {
		str = fmt.Sprintf("%s src: %v", str, prettyBytes(t.src))
	}
	if t.dest > 0 {
		str = fmt.Sprintf("%s dest: %v", str, prettyBytes(t.dest))
	}
	return str
}

// Resolves the local path relative to the root directory
// Returns the path relative to the remote, the abspath on disk and an error if any
func (g *Commands) pathResolve() (relPath, absPath string, err error) {
	root := g.context.AbsPathOf("")
	absPath = g.context.AbsPathOf(g.opts.Path)
	relPath = ""

	if absPath != root {
		relPath, err = filepath.Rel(root, absPath)
		if err != nil {
			return
		}
	} else {
		var cwd string
		if cwd, err = os.Getwd(); err != nil {
			return
		}
		if cwd == root {
			relPath = ""
		} else if relPath, err = filepath.Rel(root, cwd); err != nil {
			return
		}
	}
	relPath = strings.Join([]string{"", relPath}, "/")

	return
}

func (g *Commands) changeListResolve(relToRoot, fsPath string, isPush bool) (cl []*Change, err error) {
	var r, l *File
	r, err = g.rem.FindByPath(relToRoot)
	if err != nil {
		// We cannot pull from a non-existant remote
		if !isPush || err != ErrPathNotExists {
			return
		}
	}

	localinfo, _ := os.Stat(fsPath)
	if localinfo != nil {
		l = NewLocalFile(fsPath, localinfo)
	}

	return g.resolveChangeListRecv(isPush, relToRoot, relToRoot, r, l)
}

func (g *Commands) clearMountPoints() {
	if g.opts.Mount == nil {
		return
	}
	mount := g.opts.Mount
	for _, point := range mount.Points {
		point.Unmount()
	}

	if mount.CreatedMountDir != "" {
		if rmErr := os.RemoveAll(mount.CreatedMountDir); rmErr != nil {
			fmt.Printf("clearMountPoints removing %s: %v\n", mount.CreatedMountDir, rmErr)
		}
	}
	if mount.ShortestMountRoot != "" {
		if rmErr := os.RemoveAll(mount.ShortestMountRoot); rmErr != nil {
			fmt.Printf("clearMountPoints: shortestMountRoot: %v\n", mount.ShortestMountRoot, rmErr)
		}
	}
}

func (g *Commands) differ(a, b *File) bool {
	return fileDifferences(a, b, g.opts.IgnoreChecksum) == DifferNone
}

func (g *Commands) resolveChangeListRecv(
	isPush bool, d, p string, r *File, l *File) (cl []*Change, err error) {
	var change *Change
	if isPush {
		// Handle the case of doc files for which we don't have a direct download
		// url but have exportable links. These files should not be clobbered on push
		if hasExportLinks(r) {
			return cl, nil
		}
		change = &Change{Path: p, Src: l, Dest: r, Parent: d}
	} else {
		if !g.opts.Force && hasExportLinks(r) {
			// The case when we have files that don't provide the download urls
			// but exportable links, we just need to check that mod times are the same.
			mask := fileDifferences(r, l, g.opts.IgnoreChecksum)
			if !dirTypeDiffers(mask) && !modTimeDiffers(mask) {
				return cl, nil
			}
		}
		change = &Change{Path: p, Src: r, Dest: l, Parent: d}
	}

	change.Force = g.opts.Force
	change.NoClobber = g.opts.NoClobber
	change.IgnoreChecksum = g.opts.IgnoreChecksum

	if change.Op() != OpNone {
		cl = append(cl, change)
	}
	if !g.opts.Recursive {
		return cl, nil
	}

	// TODO: handle cases where remote and local type don't match
	if !isPush && r != nil && !r.IsDir {
		return cl, nil
	}
	if isPush && l != nil && !l.IsDir {
		return cl, nil
	}

	// look-up for children
	var localChildren chan *File
	if l == nil {
		localChildren = make(chan *File)
		close(localChildren)
	} else {
		localChildren, err = list(g.context, p, g.opts.Hidden)
		if err != nil {
			return
		}
	}

	var remoteChildren chan *File
	if r != nil {
		remoteChildren = g.rem.FindByParentId(r.Id, g.opts.Hidden)
	} else {
		remoteChildren = make(chan *File)
		close(remoteChildren)
	}
	dirlist := merge(remoteChildren, localChildren)

	// Arbitrary value. TODO: Calibrate or calculate this value
	chunkSize := 100
	srcLen := len(dirlist)
	chunkCount, remainder := srcLen/chunkSize, srcLen%chunkSize
	i := 0

	if remainder != 0 {
		chunkCount += 1
	}

	var wg sync.WaitGroup
	wg.Add(chunkCount)
	spin := spinner.New(10)
	spin.Start()

	for j := 0; j < chunkCount; j += 1 {
		end := i + chunkSize
		if end >= srcLen {
			end = srcLen
		}

		go func(wg *sync.WaitGroup, isPush bool, cl *[]*Change, p string, dlist []*dirList) {
			defer wg.Done()
			for _, l := range dlist {
				// Avoiding path.Join which normalizes '/+' to '/'
				var joined string
				if p == "/" {
					joined = "/" + l.Name()
				} else {
					joined = strings.Join([]string{p, l.Name()}, "/")
				}
				childChanges, _ := g.resolveChangeListRecv(isPush, p, joined, l.remote, l.local)
				*cl = append(*cl, childChanges...)
			}
		}(&wg, isPush, &cl, p, dirlist[i:end])

		i += chunkSize
	}
	wg.Wait()
	spin.Stop()
	return cl, nil
}

func merge(remotes, locals chan *File) (merged []*dirList) {
	spin := spinner.New(1)
	spin.Start()

	localMap := map[string]*File{}

	// TODO: Add support for FileSystems that allow same names but different files.
	for l := range locals {
		localMap[l.Name] = l
	}

	for r := range remotes {
		list := &dirList{remote: r}
		// look for local
		l, ok := localMap[r.Name]
		if ok {
			list.local = l
			delete(localMap, r.Name)
		}
		merged = append(merged, list)
	}

	// if anything left in locals, add to the dir listing
	for _, l := range localMap {
		merged = append(merged, &dirList{local: l})
	}
	spin.Stop()
	return
}

func reduceToSize(changes []*Change, isPush bool) (totalSize int64) {
	totalSize = 0
	for _, c := range changes {
		if isPush {
			if c.Src != nil {
				totalSize += c.Src.Size
			}
		} else {
			if c.Dest != nil {
				totalSize += c.Dest.Size
			}
		}
	}
	return totalSize
}

func checkAndPrintChanges(changes []*Change, reduce bool) bool {
	// Firstly detect the conflicting changes and if present return false
	var conflicts []string
	for _, c := range changes {
		if c.Op() == OpModConflict {
			conflicts = append(conflicts, c.Path)
		}
	}

	conflictCount := len(conflicts)
	if conflictCount >= 1 {
		fmt.Printf("These %d file(s) would be overwritten. Use -%s to override this behaviour\n", conflictCount, ForceKey)
		for _, path := range conflicts {
			fmt.Println(path)
		}
		return false
	}

	if reduce {
		opMap := map[int]sizeCounter{}

		for _, c := range changes {
			op := c.Op()
			if op != OpNone {
				fmt.Println(c.Symbol(), c.Path)
			}
			counter := opMap[op]
			counter.count += 1
			if c.Src != nil {
				counter.src += c.Src.Size
			}
			if c.Dest != nil {
				counter.dest += c.Dest.Size
			}
			opMap[op] = counter
		}

		for op, counter := range opMap {
			if counter.count < 1 {
				continue
			}
			_, name := opToString(op)
			fmt.Printf("%s %s\n", name, counter.String())
		}
	}
	return true
}

func promptForChanges() bool {
	input := "Y"
	fmt.Print("Proceed with the changes? [Y/n]: ")
	fmt.Scanln(&input)
	return strings.ToUpper(input) == "Y"
}

func printChangeList(changes []*Change, noPrompt bool, noClobber bool) bool {
	if len(changes) == 0 {
		fmt.Println("Everything is up-to-date.")
		return false
	}
	if !checkAndPrintChanges(changes, !noPrompt) {
		return false
	}

	if noPrompt {
		return true
	}
	return promptForChanges()
}
