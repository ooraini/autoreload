package main

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

var debug = false

func debugLog(message string) {
	if debug == true {
		log.Println(message)
	}
}

// Return all directories and files under dir
func recursiveDirList(dir string, matcher *regexp.Regexp) (map[string]bool, map[string]bool) {
	directories := make(map[string]bool)
	files := make(map[string]bool)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}

		if d.IsDir() {
			directories[p] = true
		} else if matcher.MatchString(path.Base(p)) {
			files[p] = true
		}

		return nil
	})

	if err != nil {
		debugLog(fmt.Sprintf("WalkDir error: %s", err))
	}

	return directories, files
}

func main() {
	if len(os.Args) <= 2 {
		os.Exit(1)
	}

	if os.Getenv("AUTORELOAD_DEBUG") == "true" {
		debug = true
	}

	var matcher = regexp.MustCompile(".*")
	pattern := os.Getenv("AUTORELOAD_PATTERN")
	if pattern != "" {
		matcher = regexp.MustCompile(pattern)
	}

	pwd, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Printf("unable to create watcher: %s\n", err)
		os.Exit(1)
	}

	fileEvents := make(chan fsnotify.Event)

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					debugLog("closing fileEvents")
					close(fileEvents)
					return
				}
				debugLog(fmt.Sprintf("%s: %s", event.Op, event.Name))
				fileEvents <- event
			case err, ok := <-watcher.Errors:
				if !ok {
					debugLog("closing fileEvents")
					close(fileEvents)
					return
				}
				debugLog(fmt.Sprintf("error: %s", err))
			}
		}
	}()

	reloadChan := make(chan struct{})

	go func() {
		ticker := time.NewTicker(time.Second)
		var events = make(map[string]fsnotify.Event)
		dirCache, fileCache := recursiveDirList(pwd, matcher)
		fire := false
		for d, _ := range dirCache {
			_ = watcher.Add(d)
		}

		for {
			select {
			case event, ok := <-fileEvents:
				if !ok {
					debugLog("closing realoadChan")
					close(reloadChan)
					ticker.Stop()
					return
				}

				events[event.Name] = event
			case <-ticker.C:
				for _, event := range events {
					fullPath := event.Name
					name := path.Base(event.Name)

					if fileCache[fullPath] {
						fire = true
						if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
							delete(fileCache, fullPath)
						}
						debugLog(fmt.Sprintf("firing: %s", fullPath))

					} else if dirCache[fullPath] {
						fire = true
						if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
							delete(fileCache, fullPath)
						}
						debugLog(fmt.Sprintf("firing: %s", fullPath))

					} else {
						if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
							// A file that we don't care about
							continue
						}

						info, err := os.Lstat(fullPath)
						if err != nil {
							debugLog(fmt.Sprintf("lstat error: %s", err))
							continue
						}

						if info.IsDir() {
							newDirs, newFiles := recursiveDirList(fullPath, matcher)
							for p, _ := range newDirs {
								debugLog(fmt.Sprintf("caching dir: %s", fullPath))
								dirCache[p] = true
								_ = watcher.Add(p)
							}

							for p, _ := range newFiles {
								debugLog(fmt.Sprintf("caching file: %s", fullPath))
								fileCache[p] = true
							}
							fire = true
						} else {
							if matcher.MatchString(name) {
								fileCache[fullPath] = true
								fire = true
							}
						}
						debugLog(fmt.Sprintf("firing: %s", fullPath))
					}
				}

				events = make(map[string]fsnotify.Event)

				if fire {
					select {
					case reloadChan <- struct{}{}:
						debugLog("firing reload")
						fire = false
					default:
					}
				}
			}
		}
	}()

	exitChan := make(chan int)

	go func() {
		childExitChan, childKillChan, childSignalChan, err := startProcess()
		if err != nil {
			debugLog(fmt.Sprintf("start error: %s", err))
			exitChan <- 1
		}
		debugLog("child started")

		signals := make(chan os.Signal)
		signal.Notify(signals)

		for {
			select {
			case s := <-signals:
				select {
				case childSignalChan <- s:
				default:
				}
			case _, ok := <-reloadChan:
				if !ok {
					return
				}
				childKillChan <- struct{}{}
				_ = <-childExitChan
				childExitChan, childKillChan, childSignalChan, err = startProcess()
				if err != nil {
					debugLog(fmt.Sprintf("start error: %s", err))
					exitChan <- 1
				}
				debugLog("child restarted")
			case code := <-childExitChan:
				debugLog("child exited")
				exitChan <- code
				return
			}
		}
	}()

	code := <-exitChan
	watcher.Close()
	os.Exit(code)
}

func startProcess() (chan int, chan struct{}, chan os.Signal, error) {
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		return nil, nil, nil, err
	}

	waitChan := make(chan struct{}, 2)
	exitChan := make(chan int)

	go func() {
		state, err := cmd.Process.Wait()
		waitChan <- struct{}{}
		waitChan <- struct{}{}
		debugLog("wait completed")

		if err != nil {
			exitChan <- 1
		} else {
			exitChan <- state.ExitCode()
		}
		debugLog("exit go routine finished")
	}()

	killChan := make(chan struct{})
	go func() {
		select {
		case <-killChan:
			_ = cmd.Process.Signal(syscall.SIGINT)
			debugLog("SIGINT child")
			select {
			case <-waitChan:
				debugLog("SIGINT worked")
			case <-time.NewTimer(time.Second * 10).C:
				debugLog("SIGKILL child")
				_ = cmd.Process.Kill()
			}
		case <-waitChan:
			//	Exit go routine
		}
		debugLog("kill go routine finished")
	}()

	signalChan := make(chan os.Signal, 10)
	go func() {
		for {
			select {
			case s := <-signalChan:
				debugLog(fmt.Sprintf("signaling %s", s))
				_ = cmd.Process.Signal(s)
			case <-waitChan:
				debugLog("signal go routine finished")
				return
			}
		}
	}()

	return exitChan, killChan, signalChan, nil
}
