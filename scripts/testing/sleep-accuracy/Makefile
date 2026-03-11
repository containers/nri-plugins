CFLAGS := -Wall
LDLIBS := -lm -lpthread

ifdef DEBUG
CFLAGS += -O0 -g
else
CFLAGS += -O2
endif

ifdef STATIC
LDFLAGS += -static
endif

sleep-accuracy: sleep-accuracy.c
	gcc $(CFLAGS) $(LDFLAGS) -o $@ $< $(LDLIBS)

clean:
	$(RM) sleep-accuracy
