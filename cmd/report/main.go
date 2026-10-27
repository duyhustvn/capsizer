package main

import (
	"flag"
	"fmt"
	"os"

	"capsizer/pkg/analyzer"
)

func main() {
	rampPath := flag.String("ramp", "", "đường dẫn file JSONL kết quả phát tải (bắt buộc)")
	probePath := flag.String("probe", "", "đường dẫn file JSONL chỉ số hệ thống từ probe")
	flag.Parse()

	if *rampPath == "" {
		fmt.Fprintln(os.Stderr, "ERROR: bắt buộc truyền cờ -ramp <file-ramp.jsonl>")
		flag.Usage()
		os.Exit(2)
	}

	ramp, err := analyzer.LoadRamp(*rampPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: không đọc được file ramp %s: %v\n", *rampPath, err)
		os.Exit(1)
	}

	var probe []analyzer.ProbeRecord
	if *probePath != "" {
		probe, err = analyzer.LoadProbe(*probePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: không đọc được file probe %s: %v\n", *probePath, err)
			os.Exit(1)
		}
	}

	if err := analyzer.RunAnalysis(ramp, probe); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: phân tích thất bại: %v\n", err)
		os.Exit(1)
	}
}
