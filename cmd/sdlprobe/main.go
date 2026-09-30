// Command sdlprobe is the Stage 0 "SDL hello world" for the console. It
// prints what SDL sees (video driver, displays, renderers, controllers), fills
// each display with its own colour so you can tell which physical screen is
// which, then logs raw input events so buttons can be mapped.
//
//	./sdlprobe                 # 3 s per display, then 20 s of input logging
//	./sdlprobe -input 60       # log input for a minute
//	./sdlprobe -controllers    # controller mappings only, nothing on screen
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/veandco/go-sdl2/sdl"
)

var colors = []sdl.Color{
	{R: 220, G: 40, B: 40, A: 255},  // display 0: red
	{R: 40, G: 200, B: 60, A: 255},  // display 1: green
	{R: 50, G: 90, B: 230, A: 255},  // display 2: blue
	{R: 230, G: 200, B: 40, A: 255}, // display 3: yellow
}

var colorNames = []string{"RED", "GREEN", "BLUE", "YELLOW"}

func main() {
	perDisplay := flag.Int("seconds", 3, "seconds to show each display's colour")
	inputSecs := flag.Int("input", 20, "seconds to log input events (0 = skip)")
	onlyControllers := flag.Bool("controllers", false, "only list controllers and their mappings (no video, nothing on screen)")
	flag.Parse()

	if *onlyControllers {
		if err := sdl.Init(sdl.INIT_JOYSTICK | sdl.INIT_GAMECONTROLLER); err != nil {
			fmt.Println("SDL_Init failed:", err)
			os.Exit(1)
		}
		defer sdl.Quit()
		listControllers()
		return
	}

	if err := sdl.Init(sdl.INIT_VIDEO | sdl.INIT_JOYSTICK | sdl.INIT_GAMECONTROLLER); err != nil {
		fmt.Println("SDL_Init failed:", err)
		os.Exit(1)
	}
	defer sdl.Quit()

	var ver sdl.Version
	sdl.GetVersion(&ver)
	fmt.Printf("SDL runtime version: %d.%d.%d\n", ver.Major, ver.Minor, ver.Patch)

	fmt.Print("video drivers compiled in:")
	nd, _ := sdl.GetNumVideoDrivers()
	for i := 0; i < nd; i++ {
		fmt.Print(" ", sdl.GetVideoDriver(i))
	}
	driver, _ := sdl.GetCurrentVideoDriver()
	fmt.Println("\ncurrent video driver:", driver)

	nr, _ := sdl.GetNumRenderDrivers()
	for i := 0; i < nr; i++ {
		var info sdl.RendererInfo
		if _, err := sdl.GetRenderDriverInfo(i, &info); err == nil {
			fmt.Printf("render driver %d: %s flags=0x%x\n", i, info.Name, info.Flags)
		}
	}

	n, err := sdl.GetNumVideoDisplays()
	if err != nil {
		fmt.Println("GetNumVideoDisplays:", err)
	}
	fmt.Println("displays:", n)
	for i := 0; i < n; i++ {
		name, _ := sdl.GetDisplayName(i)
		b, _ := sdl.GetDisplayBounds(i)
		m, _ := sdl.GetCurrentDisplayMode(i)
		ddpi, hdpi, vdpi, _ := sdl.GetDisplayDPI(i)
		fmt.Printf("  display %d %q bounds=%+v mode=%dx%d@%d dpi=%.0f/%.0f/%.0f\n",
			i, name, b, m.W, m.H, m.RefreshRate, ddpi, hdpi, vdpi)
		nm, _ := sdl.GetNumDisplayModes(i)
		for j := 0; j < nm && j < 8; j++ {
			dm, _ := sdl.GetDisplayMode(i, j)
			fmt.Printf("    mode %d: %dx%d@%d\n", j, dm.W, dm.H, dm.RefreshRate)
		}
	}

	for i := 0; i < n && i < len(colors); i++ {
		showDisplay(i, *perDisplay)
	}

	listControllers()
	if *inputSecs > 0 {
		logInput(time.Duration(*inputSecs) * time.Second)
	}
}

func showDisplay(i, seconds int) {
	b, err := sdl.GetDisplayBounds(i)
	if err != nil {
		fmt.Println("bounds:", err)
		return
	}
	w, err := sdl.CreateWindow(fmt.Sprintf("display %d", i), b.X, b.Y, b.W, b.H,
		sdl.WINDOW_SHOWN|sdl.WINDOW_FULLSCREEN)
	if err != nil {
		fmt.Printf("display %d: CreateWindow: %v\n", i, err)
		return
	}
	defer w.Destroy()
	r, err := sdl.CreateRenderer(w, -1, sdl.RENDERER_ACCELERATED|sdl.RENDERER_PRESENTVSYNC)
	if err != nil {
		fmt.Printf("display %d: CreateRenderer: %v (trying software)\n", i, err)
		if r, err = sdl.CreateRenderer(w, -1, sdl.RENDERER_SOFTWARE); err != nil {
			fmt.Printf("display %d: software renderer: %v\n", i, err)
			return
		}
	}
	defer r.Destroy()
	if info, err := r.GetInfo(); err == nil {
		fmt.Printf("display %d: renderer %s\n", i, info.Name)
	}
	idx, _ := w.GetDisplayIndex()
	ww, wh := w.GetSize()
	fmt.Printf("display %d: window landed on display %d, size %dx%d -> showing %s for %ds\n",
		i, idx, ww, wh, colorNames[i], seconds)

	c := colors[i]
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		for e := sdl.PollEvent(); e != nil; e = sdl.PollEvent() {
		}
		r.SetDrawColor(c.R, c.G, c.B, 255)
		r.Clear()
		// A white bar per display index, so the colour is not the only cue.
		r.SetDrawColor(255, 255, 255, 255)
		for k := 0; k <= i; k++ {
			r.FillRect(&sdl.Rect{X: 40 + int32(k)*60, Y: 40, W: 40, H: 160})
		}
		r.Present()
		sdl.Delay(16)
	}
}

func listControllers() {
	num := sdl.NumJoysticks()
	fmt.Println("joysticks:", num)
	for i := 0; i < num; i++ {
		guid := sdl.JoystickGetDeviceGUID(i)
		fmt.Printf("  joystick %d: %q guid=%s gamecontroller=%v\n",
			i, sdl.JoystickNameForIndex(i), sdl.JoystickGetGUIDString(guid), sdl.IsGameController(i))
		if sdl.IsGameController(i) {
			fmt.Printf("    mapping: %s\n", sdl.GameControllerMappingForGUID(guid))
		}
		js := sdl.JoystickOpen(i)
		if js != nil {
			fmt.Printf("    buttons=%d axes=%d hats=%d\n", js.NumButtons(), js.NumAxes(), js.NumHats())
		}
		if sdl.IsGameController(i) {
			sdl.GameControllerOpen(i)
		}
	}
}

func logInput(d time.Duration) {
	fmt.Printf("press every button, d-pad direction and stick for %s...\n", d)
	// A window is needed on some drivers to receive keyboard events.
	w, _ := sdl.CreateWindow("input", 0, 0, 320, 240, sdl.WINDOW_SHOWN)
	if w != nil {
		defer w.Destroy()
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for e := sdl.PollEvent(); e != nil; e = sdl.PollEvent() {
			switch ev := e.(type) {
			case *sdl.KeyboardEvent:
				if ev.Type == sdl.KEYDOWN {
					fmt.Printf("KEY     down sym=%d (%s) scancode=%d\n", ev.Keysym.Sym, sdl.GetKeyName(ev.Keysym.Sym), ev.Keysym.Scancode)
				}
			case *sdl.ControllerButtonEvent:
				if ev.Type == sdl.CONTROLLERBUTTONDOWN {
					fmt.Printf("CTRL    button %d (%s)\n", ev.Button, sdl.GameControllerGetStringForButton(sdl.GameControllerButton(ev.Button)))
				}
			case *sdl.ControllerAxisEvent:
				if ev.Value > 16000 || ev.Value < -16000 {
					fmt.Printf("CTRL    axis %d (%s) = %d\n", ev.Axis, sdl.GameControllerGetStringForAxis(sdl.GameControllerAxis(ev.Axis)), ev.Value)
				}
			case *sdl.JoyButtonEvent:
				if ev.Type == sdl.JOYBUTTONDOWN {
					fmt.Printf("JOY     button %d\n", ev.Button)
				}
			case *sdl.JoyHatEvent:
				fmt.Printf("JOY     hat %d = %d\n", ev.Hat, ev.Value)
			case *sdl.JoyAxisEvent:
				if ev.Value > 16000 || ev.Value < -16000 {
					fmt.Printf("JOY     axis %d = %d\n", ev.Axis, ev.Value)
				}
			case *sdl.TouchFingerEvent:
				if ev.Type == sdl.FINGERDOWN {
					fmt.Printf("TOUCH   touch=%d x=%.3f y=%.3f\n", ev.TouchID, ev.X, ev.Y)
				}
			case *sdl.QuitEvent:
				return
			}
		}
		sdl.Delay(10)
	}
}
