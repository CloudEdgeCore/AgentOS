;; Deterministic conformance component core module for the agent world
;; (runtime/wasm/wit/agent.wit). The module implements the Standard32 "direct"
;; ABI of
;;
;;   run: func(input: string) -> result<string, string>
;;
;; which the wit-component adapter encodes as:
;;
;;   cm32p2||run(input_ptr, input_len) -> retptr
;;   cm32p2||run_post(retptr)
;;   cm32p2_memory
;;   cm32p2_realloc(old_ptr, old_size, align, new_size) -> ptr
;;   cm32p2_initialize()
;;
;; The callee allocates the retptr area (12 bytes: i32 discriminant, then the
;; ok-payload string as ptr/len) with the bump allocator and returns its
;; address; run_post is a no-op because the sandbox instance is discarded
;; after every run. The ok payload is a verbatim copy of the input.
(module
  (memory (export "cm32p2_memory") 1)

  ;; Bump allocator cursor inside the module memory.
  (global $free (mut i32) (i32.const 0))

  (func $cm32p2_realloc (export "cm32p2_realloc")
      (param $old i32) (param $old_size i32) (param $align i32) (param $new_size i32)
      (result i32)
    (local $aligned i32)
    (local $end i32)
    (local $bytes_needed i32)
    (local $pages i32)
    ;; aligned = (free + align - 1) & -align  (align is a power of two)
    (local.set $aligned
      (i32.and
        (i32.add (global.get $free) (i32.sub (local.get $align) (i32.const 1)))
        (i32.sub (i32.const 0) (local.get $align))))
    (local.set $end (i32.add (local.get $aligned) (local.get $new_size)))
    (block $fits
      (br_if $fits
        (i32.le_u (local.get $end)
          (i32.mul (memory.size) (i32.const 65536))))
      (local.set $bytes_needed
        (i32.sub (local.get $end) (i32.mul (memory.size) (i32.const 65536))))
      (local.set $pages
        (i32.div_u
          (i32.add (local.get $bytes_needed) (i32.const 65535))
          (i32.const 65536)))
      (drop (memory.grow (local.get $pages))))
    (global.set $free (local.get $end))
    (local.get $aligned))

  (func (export "cm32p2_initialize"))

  (func $run (export "cm32p2||run")
      (param $input_ptr i32) (param $input_len i32)
      (result i32)
    (local $retptr i32)
    (local $out_ptr i32)
    (local $offset i32)
    (local.set $retptr
      (call $cm32p2_realloc
        (i32.const 0) (i32.const 0) (i32.const 4) (i32.const 12)))
    (local.set $out_ptr
      (call $cm32p2_realloc
        (i32.const 0) (i32.const 0) (i32.const 4) (local.get $input_len)))
    (block $copied
      (loop $copy
        (br_if $copied (i32.ge_u (local.get $offset) (local.get $input_len)))
        (i32.store8
          (i32.add (local.get $out_ptr) (local.get $offset))
          (i32.load8_u (i32.add (local.get $input_ptr) (local.get $offset))))
        (local.set $offset (i32.add (local.get $offset) (i32.const 1)))
        (br $copy)))
    (i32.store (local.get $retptr) (i32.const 0))
    (i32.store offset=4 (local.get $retptr) (local.get $out_ptr))
    (i32.store offset=8 (local.get $retptr) (local.get $input_len))
    (local.get $retptr))

  (func (export "cm32p2||run_post")
      (param $retptr i32))
)
