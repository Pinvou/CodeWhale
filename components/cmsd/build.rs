fn main() {
    // r11 P2-12：CMSD_BUILD_REVISION 经 option_env! 烘焙，但 cargo 指纹
    // 默认不追踪环境变量——持久 target 下 HEAD 移动会沿用旧值，
    // validate_binary_info 假失败。声明依赖让 cargo 在该值变化时重建。
    println!("cargo:rerun-if-env-changed=CMSD_BUILD_REVISION");
}
