// Command usagewatch samples per-interface traffic and recommends a data plan.
//
// It answers the only question that costs money on a metered link: how large a
// bundle you actually need, and how much of that need pre-staging would erase.
// It watches rather than guesses, because real usage is the only honest input.
//
// Counters come from /proc/net/dev. A map from interface to "metered" tells it
// which bytes cost money; the deferrable share is read from mobilelinkd's status
// endpoint when one is reachable, so the saving reflects real policy.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/s-rakim/my-personal-project/mobile/internal/budget"
)

func main() {
	meteredList := flag.String("metered", "",
		"comma-separated metered interfaces, e.g. wwan0,usb0 (cellular, satellite)")
	interval := flag.Duration("interval", 5*time.Minute, "how often to sample")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	out := flag.String("out", "", "append samples to this file as JSON lines, to survive a restart")
	analyseFile := flag.String("analyse", "",
		"skip sampling; read JSON-line samples from this file and report")
	flag.Parse()

	plans := defaultPlans()

	if *analyseFile != "" {
		if err := analyseOnly(*analyseFile, plans); err != nil {
			fmt.Fprintf(os.Stderr, "usagewatch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	metered := make(map[string]bool)
	for _, name := range strings.Split(*meteredList, ",") {
		if name = strings.TrimSpace(name); name != "" {
			metered[name] = true
		}
	}
	if len(metered) == 0 {
		fmt.Fprintln(os.Stderr,
			"usagewatch: no metered interfaces given. Without --metered, nothing is\n"+
				"            billed and the recommendation is trivially the smallest plan.\n"+
				"            Example: usagewatch --metered wwan0")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var sink *os.File
	if *out != "" {
		var err error
		sink, err = os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			fmt.Fprintf(os.Stderr, "usagewatch: open %s: %v\n", *out, err)
			os.Exit(1)
		}
		defer sink.Close()
	}

	fmt.Printf("sampling every %s; Ctrl-C to stop and report\n", *interval)
	var samples []budget.Sample

	record := func() {
		readings, err := readProcNetDev(metered)
		if err != nil {
			fmt.Fprintf(os.Stderr, "usagewatch: %v\n", err)
			return
		}
		for _, s := range readings {
			samples = append(samples, s)
			if sink != nil {
				if raw, err := json.Marshal(s); err == nil {
					fmt.Fprintln(sink, string(raw))
				}
			}
		}
	}

	record()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			record()
		}
	}

	fmt.Println()
	report(samples, plans)
}

func analyseOnly(path string, plans []budget.Plan) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var samples []budget.Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s budget.Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return fmt.Errorf("parse %q: %w", line, err)
		}
		samples = append(samples, s)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	report(samples, plans)
	return nil
}

func report(samples []budget.Sample, plans []budget.Plan) {
	r, err := budget.Analyse(samples, plans)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usagewatch: %v\n", err)
		os.Exit(1)
	}

	const gb = 1e9
	fmt.Printf("Observed %.1f hours (%s confidence)\n",
		r.Observed.Hours(), strings.SplitN(r.Confidence, ":", 2)[0])
	fmt.Printf("  metered traffic     %.2f GB  (%.2f GB of it deferrable)\n",
		float64(r.MeteredBytes)/gb, float64(r.DeferrableOnMetered)/gb)
	fmt.Printf("  free-link traffic   %.2f GB\n", float64(r.FreeBytes)/gb)
	fmt.Println()
	fmt.Printf("Projected per month\n")
	fmt.Printf("  as-is               %.1f GB\n", r.ProjectedMeteredGBPerMonth)
	fmt.Printf("  with pre-staging    %.1f GB   (saves %.1f GB)\n",
		r.ProjectedWithPrestagingGBPerMonth, r.SavingsGBPerMonth())
	fmt.Println()
	fmt.Printf("Recommended plan\n")
	fmt.Printf("  without pre-staging  %-28s %.2f/mo\n",
		r.Recommendation.WithoutPrestaging.Name, r.Recommendation.WithoutPrestaging.PriceMo)
	fmt.Printf("  with pre-staging     %-28s %.2f/mo\n",
		r.Recommendation.WithPrestaging.Name, r.Recommendation.WithPrestaging.PriceMo)
	fmt.Printf("\n  %s\n", r.Recommendation.Note)
	fmt.Printf("  confidence: %s\n", r.Confidence)
}

// defaultPlans is a generic ladder. Replace with the bundles your carrier
// actually sells; the recommendation is only as good as this list.
func defaultPlans() []budget.Plan {
	p := []budget.Plan{
		{Name: "1 GB", CapGB: 1, PriceMo: 3},
		{Name: "5 GB", CapGB: 5, PriceMo: 8},
		{Name: "10 GB", CapGB: 10, PriceMo: 12},
		{Name: "20 GB", CapGB: 20, PriceMo: 20},
		{Name: "50 GB", CapGB: 50, PriceMo: 40},
		{Name: "100 GB", CapGB: 100, PriceMo: 70},
	}
	sort.Slice(p, func(i, j int) bool { return p[i].CapGB < p[j].CapGB })
	return p
}
