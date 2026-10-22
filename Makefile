.PHONY: all build clean

NAME := cpa-plugin-smart-scheduler

all: build

build:
	go build -buildmode=c-shared -o $(NAME).so .

clean:
	rm -f $(NAME).so $(NAME).h
