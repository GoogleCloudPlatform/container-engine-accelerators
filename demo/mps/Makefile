CUDA_BIN_PATH=/usr/local/cuda/bin
CC=$(CUDA_BIN_PATH)/nvcc
CUDA_MEM_GET_INFO=cuda_mem_get_info
LINK_OPTS=

all: $(CUDA_MEM_GET_INFO)

$(CUDA_MEM_GET_INFO):
	$(CC) $@.c -o $@ $+ $(LINK_OPTS)

clean:
	rm -f $(CUDA_MEM_GET_INFO)
