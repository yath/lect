package main

// serialPort represents an emulated serial port that transmits data via a GPIO pin as it read from
// inputFifo.
type serialPort struct {
	id        string // Subsection in the config, used as identifier in log messages
	inputFIFO string // Named pipe to read from. Will be created by readAndProcess().
	baudRate  int    // Baud rate
	g         gpio   // Transmit GPIO. Must be configured as active-low.
}
