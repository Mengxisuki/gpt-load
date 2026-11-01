#!/usr/bin/python3
"""Oak专用打包与受限发布入口；不读取或更新NewApi的任何文件。

本地pack只组合可信构建产物，允许在macOS或Linux执行。
远端脚本由root安装，应用账号无法覆盖它或服务单元。
专用SSH公钥固定执行此脚本，不提供交互Shell或端口转发。
发布请求携带版本、源码SHA及包摘要，均先校验再接触服务。
归档始终视为不可信输入，只按固定白名单写普通文件。
原始USTAR逐头读取，拒绝隐式扩展头和隐藏归档成员。
输入包和完整解压流都有体积上限，传输另有五分钟超时。
应用二进制只由非登录应用用户执行，不能继承root权限。
SQLite与凭据加密文件必须在同一停服窗口备份。
新程序一旦尝试启动，就不能假定数据库仍适合旧版本。
失败保留版本、输入包和恢复点，人工决定修复前进或成套恢复。
当前仅支持本机回环实例，不配置公网域名或反向代理。
服务器没有源码检出，发布身份只通过随包元数据传递。
备份和发布目录由root拥有，应用仅能写入自己的数据目录。
安装脚本、服务单元和环境配置不允许通过发布包替换。
本地校验不会执行归档里的二进制，只检查其平台头。
真正执行发生在服务端，并由runuser降低到应用权限。
受限密钥不能读取管理密钥或备份内容，也不能调用恢复命令。
日志只报告发布身份、备份位置及故障，不打印管理接口内容。
人工恢复须同时评估程序、SQLite、环境配置与加密密钥。
"""

import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

# 所有生产路径固定，不允许SSH调用者通过参数或环境切换目标服务。
BASE = Path("/opt/gpt-load")
STATE = Path("/var/lib/gpt-load")
CONFIG = Path("/etc/gpt-load/gpt-load.env")
BACKUPS = Path("/var/backups/gpt-load")
URL = "http://127.0.0.1:3001"
MAX_BYTES = 384 * 1024 * 1024
# 平铺许可文件，提取时不采用归档路径、属主或权限。
FILES = {
    "gpt-load": None,
    "LICENSE": "LICENSE",
    "THIRD_PARTY_NOTICES.md": "THIRD_PARTY_NOTICES.md",
    "Apache-2.0.txt": "LICENSES/Apache-2.0.txt",
    "Inno-Setup.txt": "LICENSES/Inno-Setup.txt",
    "MIT.txt": "LICENSES/MIT.txt",
    "MPL-2.0.txt": "LICENSES/MPL-2.0.txt",
    "VERSION": None,
    "REVISION": None,
}


def validate_identity(version, revision):
    """校验不可变发布身份，不允许客户端指定任意目录名称。

完整SHA用于追溯源码，前十二位进入人可读的版本目录。
运行编号和尝试编号保证重跑不会原地覆盖旧版本。
此处不执行命令，也不依赖当前Git工作区是否存在。
"""
    # 发布标识必须对应同一提交，禁止路径、空格及Shell元字符。
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("提交SHA必须为40位小写十六进制")
    if not re.fullmatch(r"oak-" + revision[:12] + r"-[0-9]+-[0-9]+", version):
        raise ValueError("发布标识必须与提交SHA前缀对应")


def pack(binary, version, revision, archive):
    """将二进制、许可和版本信息组成服务器唯一接受的格式。

源码和前端构建由调用方完成，打包本身不下载或编译。
归档成员使用固定短名称，不产生PAX或长文件名扩展。
输出目录可由本地CI选择，远端SSH命令不暴露此参数。
每次打包后立即调用真实接收校验器，保持两端格式一致。
校验文件采用相对文件名，下载到其他临时目录后仍可验证。
环境配置、管理员凭据及运行数据均不属于发布内容。
"""
    # CI和首次人工部署使用同一打包格式，不包含数据库或密钥。
    validate_identity(version, revision)
    repo = Path(__file__).resolve().parents[2]
    if binary.is_symlink() or not binary.is_file():
        raise ValueError("二进制必须是普通文件")
    archive.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, "w:gz", format=tarfile.USTAR_FORMAT) as output:
        for name, source in FILES.items():
            if name in ("VERSION", "REVISION"):
                # 清单与二进制一同进入摘要覆盖，禁止远端猜测版本。
                value = version if name == "VERSION" else revision
                data = (value + "\n").encode("ascii")
                member = tarfile.TarInfo(name)
                member.size = len(data)
                output.addfile(member, io.BytesIO(data))
            else:
                path = binary if name == "gpt-load" else repo / source
                output.add(path, arcname=name, recursive=False)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    validate(archive, version, revision, digest)
    archive.with_suffix(archive.suffix + ".sha256").write_text(digest + "  " + archive.name + "\n")


def validate(archive, version, revision, digest, destination=None):
    """校验有界USTAR流；可选提取只写固定白名单，不调用extractall。

先检查压缩文件的完整摘要，再处理任何归档元数据。
文件清单、成员类型、平台头、版本和填充必须全部满足合同。
重复成员即使内容相同也拒绝，避免后写覆盖改变最终结果。
成员大小先检查，再按固定小块复制，不按攻击者声明分配内存。
保留较长线性流程，以便对照头、内容、填充的读取顺序审查。
提取目标只能由已校验的生产目录创建逻辑提供。
先完整验证再提取；提取中失败的未启动目录不会成为current。
不恢复归档提供的权限、属主、时间、链接及扩展属性。
返回成功表示格式正确，不表示已运行程序或完成数据库迁移。
"""
    validate_identity(version, revision)
    if not re.fullmatch(r"[0-9a-f]{64}", digest) or archive.stat().st_size > MAX_BYTES:
        raise ValueError("发布包摘要或体积非法")
    actual = hashlib.sha256()
    with archive.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            actual.update(block)
    if actual.hexdigest() != digest:
        raise ValueError("发布包SHA-256不匹配")
    seen, total = set(), 0
    with gzip.open(archive, "rb") as stream:
        while True:
            header = stream.read(512)
            total += len(header)
            if header == bytes(512):
                # 结束标记后只允许有限零填充，拒绝第二段隐藏归档与压缩炸弹。
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    total += len(block)
                    if total > MAX_BYTES or block.strip(b"\0"):
                        raise ValueError("归档尾部或解压体积非法")
                break
            if len(header) != 512 or header[257:263] != b"ustar\0":
                raise ValueError("仅接受完整USTAR普通文件头")
            member = tarfile.TarInfo.frombuf(header, "utf-8", "strict")
            if member.name not in FILES or member.name in seen:
                raise ValueError("归档有额外、重复或非法路径")
            if member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or member.linkname:
                raise ValueError("归档禁止链接、目录及特殊成员")
            padded = (member.size + 511) // 512 * 512
            if member.size <= 0 or total + padded > MAX_BYTES:
                raise ValueError("成员为空或完整解压流超限")
            metadata = {"VERSION": version, "REVISION": revision}.get(member.name)
            if metadata is not None and member.size != len(metadata) + 1:
                raise ValueError("版本清单长度不匹配")
            # 根目录由可信部署器创建，客户端不能提供提取位置。
            path = destination / member.name if destination else None
            output = path.open("xb") if path else None
            try:
                remaining = member.size
                while remaining:
                    block = stream.read(min(1024 * 1024, remaining))
                    if not block:
                        raise ValueError("归档内容被截断")
                    if remaining == member.size and member.name == "gpt-load":
                        if len(block) < 20 or block[:6] != b"\x7fELF\x02\x01" or block[18:20] != b"\x3e\x00":
                            raise ValueError("只允许Linux amd64二进制")
                    if metadata is not None and block != (metadata + "\n").encode("ascii"):
                        raise ValueError("版本清单与发布请求不匹配")
                    if output:
                        output.write(block)
                    remaining -= len(block)
            finally:
                if output:
                    output.close()
            if stream.read(padded - member.size) != bytes(padded - member.size):
                raise ValueError("归档填充非法或截断")
            if path:
                # 不恢复包内UID、模式或扩展属性，程序只读且由root拥有。
                path.chmod(0o755 if member.name == "gpt-load" else 0o644)
            total += padded
            seen.add(member.name)
    if seen != set(FILES):
        raise ValueError("归档缺少必需文件")


def run(*args):
    """执行固定系统命令并返回文本，失败不能被当作空结果。

应用密钥永远不进入这些命令参数，避免进程列表泄露。
输出由调用方按语义消费，不把整段管理接口响应写到CI。
每个系统命令有超时，避免systemd异常导致部署锁无限占用。
"""
    # 所有系统命令使用独立参数且不用Shell，错误向上传递。
    result = subprocess.run(args, check=True, capture_output=True, text=True, timeout=90)
    return result.stdout.strip()


def probe(version):
    """健康、真实前端、认证和数据库读接口必须共同可用。

验证固定回环地址，不能由SSH调用者指定探测目标。
健康接口只确认进程版本，不能替代数据库读取检查。
首页为二〇〇也可能只是未构建占位页，必须检查真实入口。
匿名管理请求必须被拒绝，持久管理密钥则应正常认证。
验证仅执行读取，不创建生产渠道、账号或访问密钥。
当前约定管理密钥由应用生成到数据目录，环境模板保持空值。
网络及解析错误向外报告，不以静默降级掩盖启动故障。
"""
    # 服务必须保持回环监听；部署不得悄悄将管理面开放到公网。
    listeners = run("ss", "-H", "-lnt", "sport = :3001").splitlines()
    if not listeners or any(line.split()[3] != "127.0.0.1:3001" for line in listeners):
        raise ValueError("3001端口必须仅监听127.0.0.1")
    with urllib.request.urlopen(URL + "/health", timeout=10) as response:
        health = json.load(response)
    if health.get("status") != "ok" or health.get("version") != version:
        raise ValueError("运行版本或健康状态与目标不匹配")
    with urllib.request.urlopen(URL + "/", timeout=10) as response:
        html = response.read().decode("utf-8")
    # 只有health为200不代表前端已构建，必须验证嵌入入口和真实JS文件。
    script = re.search(r'src="(/assets/[^"?]+\.js)"', html)
    if '<div id="app"></div>' not in html or not script:
        raise ValueError("前端构建资源缺失")
    with urllib.request.urlopen(URL + script[1], timeout=10) as response:
        if not response.read(128):
            raise ValueError("前端脚本为空")
    try:
        urllib.request.urlopen(URL + "/api/auth/session", timeout=10)
    except urllib.error.HTTPError as error:
        if error.code != 401:
            raise
    else:
        raise ValueError("管理接口允许匿名访问")
    # 密钥只用于本机鉴权，不进入命令行、日志或CI输出。
    key = (STATE / "auth.key").read_text().strip()
    for endpoint in ("/api/auth/session", "/api/groups"):
        request = urllib.request.Request(URL + endpoint, headers={"Authorization": "Bearer " + key})
        with urllib.request.urlopen(request, timeout=10) as response:
            payload = json.load(response)
        # GPT-Load使用code=0；不能误用NewApi的success布尔合同。
        if payload.get("code") != 0:
            raise ValueError("本机管理会话或数据库读取失败")


def check():
    """核对已安装实例，是首次部署后CI调用的只读前置条件。

当前链接必须解析到本应用releases下的直接子目录。
服务、环境文件及运行版本需同时存在，不代替首次安装。
不修复所有者、目录权限或缺失配置，以免预检产生隐式写入。
输出仅包含公开版本标识，不返回管理员或渠道凭据。
成功返回当前发布目录，作为备份记录与失败恢复的依据。
"""
    # 只读预检不更改持久目录、配置、密钥和数据库。
    if os.geteuid() != 0:
        raise ValueError("生产预检与发布必须由受限root入口执行")
    current = (BASE / "current").resolve(strict=True)
    if current.parent != BASE / "releases" or not CONFIG.is_file():
        raise ValueError("发布目录或配置文件不满足独立部署约定")
    run("systemctl", "is-active", "--quiet", "gpt-load")
    version = (current / "VERSION").read_text().strip()
    probe(version)
    print("GPT-Load预检通过：" + version)
    return current


def switch(target):
    """原子替换current；调用方持锁并决定是否允许恢复旧版本。

临时链接位于应用根目录，不受应用用户写权限控制。
先建立新链接再replace，避免服务看到一段current缺失窗口。
目标路径来自固定根目录与已验证版本，不接受任意外部路径。
"""
    # 在同一文件系统内原子替换链接，不覆盖正在执行的文件。
    # 独立临时目录避免中断残留阻塞下一次发布；退出时只清理自己的链接。
    with tempfile.TemporaryDirectory(prefix=".switch-", dir=BASE) as work:
        temporary = Path(work) / "current"
        temporary.symlink_to(target)
        temporary.replace(BASE / "current")


def interrupted(signum, _frame):
    """把SSH中断转换为异常，确保停服阶段经过同一恢复边界。

未启动新版本时可以恢复旧服务；已经启动时保留停服状态。
信号仅中断本次发布，不操作其他实例或重置持久文件。
SIGALRM也走这里，避免收包超时后留下无诊断的结束状态。
"""
    raise InterruptedError("发布被信号中断：" + str(signum))


def deploy(version, revision, digest):
    """完整收包校验后停服备份；尝试迁移后失败不自动降级。

同一实例的所有发布路径共用Linux文件锁。
持锁后再次预检，不能依赖CI较早执行的check结果。
接收和格式验证阶段旧服务仍正常运行。
新目录不可复用，且程序版本需以应用身份实际执行确认。
服务停止后才复制整个数据目录，包含SQLite的WAL相关文件。
环境模板虽无明文密钥，仍与运行状态一起保存作完整恢复点。
只有备份命令全部成功，才允许切换链接及启动新程序。
started在调用systemd之前标记，覆盖启动失败但迁移已发生的情形。
成功必须同时满足服务活跃及完整只读探测，不能只看start退出码。
恢复逻辑位于同一个异常边界，确保中断也遵守迁移后的降级限制。
保留较长函数是为了集中呈现停服、备份、迁移及失败恢复的顺序。
此入口不提供任意Shell、配置覆盖、备份删除或数据库恢复能力。
"""
    import fcntl  # 打包也可在macOS执行；生产锁只在Linux服务器使用。

    if os.geteuid() != 0:
        raise ValueError("部署必须由root受限入口执行")
    # 锁覆盖接收、备份、切换与验收，避免两个CI同时修改实例。
    with (BASE / ".deploy.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        previous = check()
        target = BASE / "releases" / version
        if target.exists() or target.is_symlink():
            raise ValueError("发布目录已存在，禁止覆盖")
        stopped, started = False, False
        backup = BACKUPS / version
        with tempfile.TemporaryDirectory(prefix=".deploy-", dir=BASE) as work:
            archive = Path(work) / "release.tar.gz"
            # 传输受超时和体积限制；多读一字节识别超限而非截成合法前缀。
            signal.alarm(300)
            with archive.open("xb") as output:
                received = 0
                while True:
                    block = sys.stdin.buffer.read(min(1024 * 1024, MAX_BYTES + 1 - received))
                    if not block:
                        break
                    received += len(block)
                    if received > MAX_BYTES:
                        raise ValueError("输入发布包超过384MiB")
                    output.write(block)
            signal.alarm(0)
            validate(archive, version, revision, digest)
            target.mkdir(mode=0o755)
            target.chmod(0o755)  # 默认umask保护备份，但发布目录必须允许应用用户遍历。
            validate(archive, version, revision, digest, target)
            # 即使专用SSH key被滥用，二进制也仅以非特权应用用户执行。
            reported = run("runuser", "-u", "gpt-load", "--", str(target / "gpt-load"), "version")
            if reported != "GPT-Load " + version + " (linux/amd64)":
                raise ValueError("二进制内嵌版本与清单不一致")
            try:
                stopped = True
                run("systemctl", "stop", "gpt-load")
                # SQLite WAL与加密密钥必须在同一停服窗口成套保存。
                BACKUPS.mkdir(mode=0o700, parents=True, exist_ok=True)
                backup.mkdir(mode=0o700)
                run("tar", "-czf", str(backup / "state.tar.gz"), "-C", str(STATE), ".")
                shutil.copy2(CONFIG, backup / "gpt-load.env")
                (backup / "previous-release").write_text(str(previous) + "\n")
                # 保留输入包方便追溯，不让应用用户写入发布归档。
                shutil.copy2(archive, backup / "release.tar.gz")
                switch(target)
                started = True  # 调用start即可能迁移，不能只按start返回值判断。
                run("systemctl", "start", "gpt-load")
                failure = None
                for _ in range(30):
                    try:
                        run("systemctl", "is-active", "--quiet", "gpt-load")
                        probe(version)
                        print("发布成功：" + version + "；备份：" + str(backup))
                        return
                    except InterruptedError:
                        # 信号中断必须进入迁移阶段恢复，不能混入网络重试后伪造成功。
                        raise
                    except (OSError, ValueError, urllib.error.URLError, subprocess.SubprocessError) as error:
                        failure = error
                        time.sleep(1)
                raise ValueError("新版本未通过健康检查：" + str(failure))
            except BaseException:
                if started:
                    # 已尝试启动则可能迁移，停服并保留备份，等待人工决定修复或恢复。
                    subprocess.run(["systemctl", "stop", "gpt-load"], timeout=90, check=False)
                    print("发布失败：禁止自动降级；备份=" + str(backup) + "；原版本=" + str(previous), file=sys.stderr)
                elif stopped:
                    # 新二进制尚未运行时，恢复旧链接和服务不会引入迁移兼容风险。
                    if (BASE / "current").resolve() != previous:
                        switch(previous)
                    run("systemctl", "start", "gpt-load")
                raise


def main():
    """区分本地打包与生产受限SSH命令，不将原命令交给Shell。

SSH_ORIGINAL_COMMAND存在时，即使为空也不能进入本地pack分支。
外部命令只有check或deploy两种形态，参数数量必须精确一致。
所有部署身份和摘要校验均在读取stdin及停服前完成。
固定路径和严格参数共同限制专用部署密钥的实际能力。
首次安装、密钥录入和灾难恢复仍由受信管理员连接执行。
"""
    # 固定SSH入口优先读取原命令，只接受check或三项严格参数的deploy。
    original = os.environ.get("SSH_ORIGINAL_COMMAND")
    args = shlex.split(original) if original is not None else sys.argv[1:]
    if args == ["check"]:
        check()
    elif len(args) == 4 and args[0] == "deploy":
        validate_identity(args[1], args[2])
        if not re.fullmatch(r"[0-9a-f]{64}", args[3]):
            raise ValueError("摘要格式非法")
        deploy(*args[1:])
    elif original is None and len(args) == 5 and args[0] == "pack":
        pack(Path(args[1]), args[2], args[3], Path(args[4]))
    else:
        raise ValueError("仅支持check、deploy <版本> <SHA> <摘要>或本地pack")


if __name__ == "__main__":
    os.umask(0o077)  # 持久备份、密钥和临时目录默认不可被其他系统用户读取。
    # 不让SSH断连绕过备份或启动后的失败处理。
    for event in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM, signal.SIGALRM):
        signal.signal(event, interrupted)
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError) as error:
        print("GPT-Load发布失败：" + str(error), file=sys.stderr)
        # 系统命令输出不包含密钥，保留stderr以便定位权限、磁盘或systemd失败。
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr.strip(), file=sys.stderr)
        sys.exit(1)
