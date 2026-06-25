package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.Comparison
import com.automation.agent.githubapi.Pr
import com.automation.agent.githubapi.PrInput
import com.automation.agent.gitrepo.Author
import com.automation.agent.notify.Message
import com.automation.agent.notify.Notifier
import org.eclipse.jgit.api.Git
import org.eclipse.jgit.lib.PersonIdent
import java.io.File
import java.nio.file.Files
import java.time.Instant
import java.time.ZoneId

/** A fake [GitHub] that records the created PR and applied labels. */
internal class FakeGitHub(
    var existing: List<Pr> = emptyList(),
    private val findErr: Throwable? = null,
    private val createErr: Throwable? = null,
    var comparison: Comparison = Comparison(),
) : GitHub {
    var created: PrInput? = null
    val labeled = mutableListOf<String>()

    override suspend fun compare(owner: String, repo: String, base: String, head: String): Comparison = comparison

    override suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String): Pr? {
        findErr?.let { throw it }
        return existing.firstOrNull { it.branch == branch }
    }

    override suspend fun createPr(owner: String, repo: String, input: PrInput): Pr {
        createErr?.let { throw it }
        created = input
        return Pr(number = 42, title = input.title, branch = input.head, headSha = "", url = "https://gh/pr/42", labels = emptyList())
    }

    override suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) {
        labeled += labels
    }
}

/** A fake [Notifier] capturing every message posted. */
internal class FakeNotifier : Notifier {
    val msgs = mutableListOf<Message>()
    override suspend fun notify(message: Message) {
        msgs += message
    }
}

/** Creates a local repo with one commit to act as the clone source. */
internal fun seedRemote(): String {
    val dir = Files.createTempDirectory("remote").toFile()
    Git.init().setDirectory(dir).call().use { git ->
        File(dir, "README.md").writeText("hi")
        git.add().addFilepattern("README.md").call()
        git.commit()
            .setMessage("init")
            .setAuthor(PersonIdent("seed", "s@x", Instant.ofEpochSecond(1), ZoneId.systemDefault()))
            .call()
    }
    return dir.path
}

internal fun applyCfg(remote: String): ApplyConfig =
    ApplyConfig(
        owner = "acme", repo = "api", cloneUrl = remote, base = "master", branch = "agent/fix", newBranch = true,
        label = "automation-agent", commitMessage = "fix", prTitle = "Fix", prBody = "auto",
        author = Author("agent", "a@x"),
    )
