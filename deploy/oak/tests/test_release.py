"""验证真实发布格式及失败边界，不连接生产服务器。

所有输入都在临时目录创建，测试不会修改部署目录或服务。
正常与异常归档均调用生产校验器，不能用测试内算法自证正确。
元数据攻击测试重算包摘要，保证真正触达归档解析层。
运行环境使用虚构ELF头，绝不执行该占位文件。
真实程序运行与持久化由部署烟测单独验证。
重写归档时保留原有许可内容，仅注入当前反例的缺陷。
成员缺失和版本错配分别验证，不能用同一失败替代全部合同。
测试目录由系统临时目录管理，结束后自动清理虚构数据。
异常断言不会调用systemctl，也不需要生产管理权限。
中断恢复用例隔离系统身份及服务命令，不启动伪造的测试程序。
实例路径、传输stdin、持久状态与备份均位于同一个临时目录。
真正的tar备份与原子链接操作保留，避免只断言模拟调用顺序。
运行密钥使用虚构文本，测试不会读取本机或服务器认证文件。
启动后中断必须抛出到部署事务，不能退化成稍后重试成功。
每项用例独立创建包，前一项损坏归档不会污染后续测试。
真实服务的健康检查、平台执行与重启验证由服务器烟测补充。
这些测试仅验证发布边界，不声称验证模型账号或商业挂靠能力。
"""

import hashlib
import importlib.util
import io
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from pathlib import Path

# 通过模块入口调用实际校验器，不在测试里复制生产校验逻辑。
SOURCE = Path(__file__).resolve().parents[1] / "release.py"
SPEC = importlib.util.spec_from_file_location("oak_release", SOURCE)
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class PackageTests(unittest.TestCase):
    """固定成员列表、平台、版本和摘要共同约束可部署产物。"""

    def setUp(self):
        # 使用隔离目录和虚构ELF头，不运行不可信测试二进制。
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.revision = "a" * 40
        self.version = "oak-" + "a" * 12 + "-123-1"
        self.binary = self.root / "binary"
        self.binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\x3e\x00" + bytes(32))
        # 打包器引用仓库真实许可证，确认必需资源确实存在。
        self.archive = self.root / "release.tar.gz"
        release.pack(self.binary, self.version, self.revision, self.archive)

    def digest(self):
        # 每次修改包后重算摘要，避免所有反例只撞上第一层摘要校验。
        return hashlib.sha256(self.archive.read_bytes()).hexdigest()

    def rewrite(self, extra=None, omit=None, version=None):
        """只改变指定成员，构造能通过外层摘要校验的恶意包。

这不是打包器实现副本：成员内容来自生产打包器的真实输出。
保留USTAR格式，避免因无关的格式差异掩盖目标失败条件。
链接成员不附带文件正文，符合tar头自身的类型语义。
"""
        # 保留正常包成员，仅构造本场景需要的异常输入。
        with tarfile.open(self.archive, "r:gz") as source:
            members = [(m, source.extractfile(m).read()) for m in source]
        with tarfile.open(self.archive, "w:gz", format=tarfile.USTAR_FORMAT) as target:
            for member, content in members:
                if member.name == omit:
                    continue
                if member.name == "VERSION" and version:
                    content = (version + "\n").encode()
                    member.size = len(content)
                target.addfile(member, io.BytesIO(content))
            if extra:
                target.addfile(extra, io.BytesIO(b"x") if extra.isfile() else None)

    def test_pack_and_extract(self):
        """正常发布可真实落盘，且只产生固定清单中的普通文件。

二进制内容保持不变，执行位由接收端恢复而非信任归档。
这个用例也校验全部第三方许可文本在仓库中真实存在。
"""
        # 正常包必须可真正提取，且二进制执行位由服务端明确设置。
        target = self.root / "extracted"
        target.mkdir()
        release.validate(self.archive, self.version, self.revision, self.digest(), target)
        self.assertEqual((target / "gpt-load").read_bytes(), self.binary.read_bytes())
        self.assertEqual((target / "gpt-load").stat().st_mode & 0o777, 0o755)
        self.assertEqual(set(p.name for p in target.iterdir()), set(release.FILES))

    def test_reject_wrong_digest(self):
        # 被替换的传输包不能进入提取和执行阶段。
        with self.assertRaises(ValueError):
            release.validate(self.archive, self.version, self.revision, "0" * 64)

    def test_reject_unsafe_members(self):
        """恶意路径、同名覆盖和符号链接是不同的提取风险。

逐个重建原始包，让先前的异常成员不影响后面的反例。
外层摘要正确，失败必须来自实际解析的成员约束。
"""
        # 分别验证穿越、重复成员与链接，不能只测一个坏文件名。
        original = self.archive.read_bytes()
        for name, kind in [("../escape", tarfile.REGTYPE), ("gpt-load", tarfile.REGTYPE), ("LICENSE", tarfile.SYMTYPE)]:
            with self.subTest(name=name, kind=kind):
                self.archive.write_bytes(original)
                member = tarfile.TarInfo(name)
                member.type = kind
                member.size = 1 if kind == tarfile.REGTYPE else 0
                member.linkname = "/etc/passwd" if kind == tarfile.SYMTYPE else ""
                self.rewrite(extra=member)
                with self.assertRaises(ValueError):
                    release.validate(self.archive, self.version, self.revision, self.digest())

    def test_reject_missing_license_and_wrong_version(self):
        # 许可证和版本清单是发布协议，缺失或错配都要显式失败。
        original = self.archive.read_bytes()
        for kwargs in [{"omit": "LICENSE"}, {"version": "unrelated-version"}]:
            self.archive.write_bytes(original)
            self.rewrite(**kwargs)
            with self.assertRaises(ValueError):
                release.validate(self.archive, self.version, self.revision, self.digest())

    def test_reject_wrong_architecture(self):
        # Mac或ARM程序不能作为Linux amd64发布包被服务器启动。
        self.binary.write_bytes(b"not-a-linux-amd64-binary")
        with self.assertRaises(ValueError):
            release.pack(self.binary, self.version, self.revision, self.archive)

    def test_reject_unrelated_revision_and_command_injection(self):
        """发布身份不能被当作路径或命令，也不能错配另一份源码。

校验不接触systemd，因此可在普通本机用户下验证失败入口。
具体摘要和版本与生产资源无关联，不泄露真实部署凭据。
"""
        # 参数校验先于任何生产动作，不接受目录分隔符和Shell语法。
        for version, revision in [("../escape", self.revision), (self.version, "b" * 40), (self.version + ";id", self.revision)]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.validate_identity(version, revision)

    def test_interrupted_switch_does_not_block_next_release(self):
        """链接切换失败不能遗留固定名称，阻塞之后所有部署。

只隔离文件系统replace的失败，链接创建与清理均运行真实实现。
失败后实际重试一次切换，确认仍可前进而非只检查没有异常。
整个场景只使用普通用户临时目录，不触碰生产current。
"""
        # 首次在创建临时链接之后模拟原子替换失败。
        target = self.root / "new-release"
        target.mkdir()
        with patch.object(release, "BASE", self.root):
            with patch.object(Path, "replace", side_effect=OSError("simulated interruption")):
                with self.assertRaises(OSError):
                    release.switch(target)
            # 真实重试证明清理覆盖了异常路径，而非只覆盖成功路径。
            release.switch(target)
        self.assertEqual((self.root / "current").resolve(), target.resolve())
        self.assertFalse(list(self.root.glob(".switch-*")))

    def test_interruption_after_start_stops_new_version_without_downgrade(self):
        """启动后终止信号必须停新服务，不能被健康重试吞掉。

只隔离系统身份、systemd和HTTP探测，收包、提取、备份与链接均真实执行。
备份用系统tar读取虚构数据，确认密钥文件与SQLite同时进入归档。
不会运行测试ELF，也不会连接生产服务或数据库。
这个场景覆盖启动后可能已迁移的恢复边界。
原版本链接不得被盲目恢复，否则会让旧程序打开可能不兼容的数据。
"""
        from contextlib import ExitStack

        # 构造一份独立的已运行实例目录与持久状态。
        releases = self.root / "releases"
        previous = releases / "previous"
        previous.mkdir(parents=True)
        (self.root / "current").symlink_to(previous)
        state = self.root / "state"
        state.mkdir()
        for name in ("gpt-load.db", "auth.key", "encryption.key"):
            (state / name).write_text("fictional-test-data")
        config = self.root / "service.env"
        config.write_text("HOST=127.0.0.1\n")
        commands = []
        real_run = release.subprocess.run

        def run_command(*args):
            # 保留真实备份命令，只有用户身份与系统服务交由测试边界替代。
            commands.append(args)
            if args[0] == "runuser":
                return "GPT-Load " + self.version + " (linux/amd64)"
            if args[0] == "tar":
                real_run(args, check=True, capture_output=True)
            return ""

        with ExitStack() as stack:
            # 所有路径和stdin都指向同一个临时fixture，不写入真实部署根目录。
            for name, value in {"BASE": self.root, "STATE": state, "CONFIG": config, "BACKUPS": self.root / "backups"}.items():
                stack.enter_context(patch.object(release, name, value))
            stack.enter_context(patch.object(release.os, "geteuid", return_value=0))
            stack.enter_context(patch.object(release, "check", return_value=previous))
            stack.enter_context(patch.object(release, "run", side_effect=run_command))
            stop = stack.enter_context(patch.object(release.subprocess, "run"))
            stack.enter_context(patch.object(release, "probe", side_effect=InterruptedError("SSH disconnected")))
            stack.enter_context(patch.object(release.sys, "stdin", SimpleNamespace(buffer=io.BytesIO(self.archive.read_bytes()))))
            with self.assertRaises(InterruptedError):
                release.deploy(self.version, self.revision, self.digest())
            # 使用实际stop调用断言，信号被重试分支吞掉时会失败。
            stop.assert_called_once_with(["systemctl", "stop", "gpt-load"], timeout=90, check=False)
        self.assertEqual((self.root / "current").resolve(), (releases / self.version).resolve())
        self.assertEqual(commands.count(("systemctl", "start", "gpt-load")), 1)
        backup = self.root / "backups" / self.version / "state.tar.gz"
        with tarfile.open(backup) as archive:
            # 密钥与数据库成套备份是恢复账号凭据的必要条件。
            self.assertTrue({"./gpt-load.db", "./auth.key", "./encryption.key"}.issubset(archive.getnames()))


if __name__ == "__main__":
    unittest.main()
